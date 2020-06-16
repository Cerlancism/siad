package renter

// TODO: Move the memory manager to its own package.

// TODO: Add functions that allow a caller to increase or decrease the base
// memory for the memory manager.

import (
	"runtime"
	"runtime/debug"
	"sync"

	"gitlab.com/NebulousLabs/Sia/build"
)

// memoryManager can handle requests for memory and returns of memory. The
// memory manager is initialized with a base amount of memory and it will allow
// up to that much memory to be requested simultaneously. Beyond that, it will
// block on calls to 'managedGetMemory' until enough memory has been returned to
// allow the request. High priority memory will be unblocked first, otherwise
// memory will be unblocked in a FIFO.
//
// The memory manager will put aside 'priorityReserve' memory for high priority
// requests. Lower priority requests will not be able to use this memory. This
// allows high priority requests in low volume to experience zero wait time even
// if there are a high volume of low priority requests.
//
// If a request is made that exceeds the base memory, the memory manager will
// block until all memory is available, and then grant the request, blocking all
// future requests for memory until the memory is returned. This allows large
// requests to go through even if there is not enough base memory.
//
// The memoryManager keeps track of how much memory has been returned since the
// last manual call to runtime.GC(). After enough memory has been returned since
// the previous manual call, the memoryManager will run a manual call to
// runtime.GC() and follow that up with a call to debug.FreeOSMemory(). This has
// been shown in production to significantly reduce the amount of RES that siad
// consumes, without a significant hit to performance.
type memoryManager struct {
	available       uint64 // Total memory remaining.
	base            uint64 // Initial memory.
	memSinceGC      uint64 // Counts allocations to trigger a manual GC.
	priorityReserve uint64 // Memory set aside for priority requests.
	underflow       uint64 // Large requests cause underflow.

	fifo         []*memoryRequest
	priorityFifo []*memoryRequest

	// The blocking channel receives a message (sent in a non-blocking way)
	// every time a request blocks for more memory. This is used in testing to
	// ensure that requests which are made in goroutines can be received in a
	// deterministic order.
	blocking chan struct{}
	mu       sync.Mutex
	stop     <-chan struct{}
}

// memoryRequest is a single thread that is blocked while waiting for memory.
type memoryRequest struct {
	amount uint64
	done   chan struct{}
}

// try will try to get the amount of memory requested from the manger, returning
// true if the attempt is successful, and false if the attempt is not.  In the
// event that the attempt is successful, the internal state of the memory
// manager will be updated to reflect the granted request.
func (mm *memoryManager) try(amount uint64, priority bool) bool {
	if mm.available >= (amount+mm.priorityReserve) || (priority && mm.available >= amount) {
		// There is enough memory, decrement the memory and return.
		mm.available -= amount
		return true
	} else if mm.available == mm.base && amount >= mm.available {
		// The amount of memory being requested is greater than the amount of
		// memory available, but no memory is currently in use. Set the amount
		// of memory available to zero and return.
		//
		// The effect is that all of the memory is allocated to this one
		// request, allowing the request to succeed even though there is
		// technically not enough total memory available for the request.
		mm.available = 0
		mm.underflow = amount - mm.base
		return true
	}
	return false
}

// Request is a blocking request for memory. The request will return when the
// memory has been acquired. If 'false' is returned, it means that the renter
// shut down before the memory could be allocated.
func (mm *memoryManager) Request(amount uint64, priority bool) bool {
	// Try to request the memory.
	mm.mu.Lock()
	shouldTry := len(mm.priorityFifo) == 0 && (priority == memoryPriorityHigh || len(mm.fifo) == 0)
	if shouldTry && mm.try(amount, priority) {
		mm.mu.Unlock()
		return true
	}

	// There is not enough memory available for this request, join the fifo.
	myRequest := &memoryRequest{
		amount: amount,
		done:   make(chan struct{}),
	}
	if priority {
		mm.priorityFifo = append(mm.priorityFifo, myRequest)
	} else {
		mm.fifo = append(mm.fifo, myRequest)
	}
	mm.mu.Unlock()

	// Send a note that a thread is now blocking. This is only used in testing,
	// to ensure that the test can have multiple threads blocking for memory
	// which block in a determinstic order.
	select {
	case mm.blocking <- struct{}{}:
	default:
	}

	// Block until memory is available or until shutdown. The thread that closes
	// the 'available' channel will also handle updating the memoryManager
	// variables.
	select {
	case <-myRequest.done:
		return true
	case <-mm.stop:
		return false
	}
}

// Return will return memory to the manager, waking any blocking threads which
// now have enough memory to proceed.
func (mm *memoryManager) Return(amount uint64) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	// Check how much memory has been returned since the last call to
	// runtime.GC(). If enough memory has been returned, call runtime.GC()
	// manually and reset the counter.
	mm.memSinceGC += amount
	if mm.memSinceGC > defaultMemory {
		runtime.GC()
		debug.FreeOSMemory()
		mm.memSinceGC = 0
	}

	// Add the remaining memory to the pool of available memory, clearing out
	// the underflow if needed.
	if mm.underflow > 0 && amount <= mm.underflow {
		// Not even enough memory has been returned to clear the underflow.
		// Reduce the underflow amount and return.
		mm.underflow -= amount
		return
	} else if mm.underflow > 0 && amount > mm.underflow {
		amount -= mm.underflow
		mm.underflow = 0
	}
	mm.available += amount

	// Sanity check - the amount of memory available should not exceed the base
	// unless the memory manager is being used incorrectly.
	if mm.available > mm.base {
		build.Critical("renter memory manager being used incorrectly, too much memory returned")
		mm.available = mm.base
	}

	// Release as many of the priority threads blocking in the fifo as possible.
	for len(mm.priorityFifo) > 0 {
		if !mm.try(mm.priorityFifo[0].amount, memoryPriorityHigh) {
			// There is not enough memory to grant the next request, meaning no
			// future requests should be checked either.
			return
		}
		// There is enough memory to grant the next request. Unblock that
		// request and continue checking the next requests.
		close(mm.priorityFifo[0].done)
		mm.priorityFifo = mm.priorityFifo[1:]
	}

	// Release as many of the threads blocking in the fifo as possible.
	for len(mm.fifo) > 0 {
		if !mm.try(mm.fifo[0].amount, memoryPriorityLow) {
			// There is not enough memory to grant the next request, meaning no
			// future requests should be checked either.
			return
		}
		// There is enough memory to grant the next request. Unblock that
		// request and continue checking the next requests.
		close(mm.fifo[0].done)
		mm.fifo = mm.fifo[1:]
	}
}

// newMemoryManager will create a memoryManager and return it.
func newMemoryManager(baseMemory uint64, priorityMemory uint64, stopChan <-chan struct{}) *memoryManager {
	return &memoryManager{
		available:       baseMemory,
		base:            baseMemory,
		priorityReserve: priorityMemory,

		blocking: make(chan struct{}, 1),
		stop:     stopChan,
	}
}
