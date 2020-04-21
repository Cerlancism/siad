package proto

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/writeaheadlog"

	"gitlab.com/NebulousLabs/Sia/modules"

	"gitlab.com/NebulousLabs/errors"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/types"
)

// testWAL is the WAL instance we're going to use across this test. This would
// typically come from the calling functions.
var (
	testWAL, _ = newTestWAL()

	// errTimeoutOnLock is returned when we timeout on getting a lock
	errTimeoutOnLock = errors.New("timeout while acquiring a lock ")
)

// StartUpdateWithTimeout acquires a lock, ensuring the caller is the only one currently
//// allowed to perform updates on this refcounter file.
func (rc *RefCounter) StartUpdateWithTimeout(timeout time.Duration) error {
	if timeout < 0 {
		rc.muUpdate.Lock()
	} else {
		if ok := rc.muUpdate.TryLockTimed(timeout); !ok {
			return errTimeoutOnLock
		}
	}
	return rc.managedStartUpdate()
}

// TestRefCounterCount tests that the Count method always returns the correct
// counter value, either from disk or from in-mem storage.
func TestRefCounterCount(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	sec := uint64(1)
	val := uint16(21)

	// set up the expected value on disk
	err := writeVal(rc.filepath, sec, val)
	if err != nil {
		t.Fatal("Failed to write a count to disk:", err)
	}

	// verify we can read it correctly
	rval, err := rc.Count(sec)
	if err != nil {
		t.Fatal("Failed to read count from disk:", err)
	}
	if rval != val {
		t.Fatalf("read wrong value from disk: expected %d, got %d", val, rval)
	}

	// check behaviour on bad sector number
	_, err = rc.Count(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// set up a temporary override
	ov := uint16(12)
	rc.newSectorCounts[sec] = ov

	// verify we can read it correctly
	rov, err := rc.Count(sec)
	if err != nil {
		t.Fatal("Failed to read count from disk:", err)
	}
	if rov != ov {
		t.Fatalf("read wrong override value from disk: expected %d, got %d", ov, rov)
	}
}

// TestRefCounterAppend tests that the Decrement method behaves correctly
func TestRefCounterAppend(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	numSec := fastrand.Uint64n(10)
	rc := testPrepareRefCounter(numSec, t)
	stats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("RefCounter creation finished successfully but the file is not accessible:", err)
	}
	err = rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Append
	u, err := rc.Append()
	if err != nil {
		t.Fatal("Failed to create an append update", err)
	}
	expectNumSec := numSec + 1
	if rc.numSectors != expectNumSec {
		t.Fatalf("append failed to properly increase the numSectors counter. Expected %d, got %d", expectNumSec, rc.numSectors)
	}
	if rc.newSectorCounts[rc.numSectors-1] != 1 {
		t.Fatalf("append failed to properly initialise the new coutner. Expected 1, got %d", rc.newSectorCounts[rc.numSectors-1])
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply append update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}

	// verify: we expect the file size to have grown by 2 bytes
	endStats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("Failed to get file stats:", err)
	}
	expectSize := stats.Size() + 2
	actualSize := endStats.Size()
	if actualSize != expectSize {
		t.Fatalf("File size did not grow as expected. Expected size: %d, actual size: %d", expectSize, actualSize)
	}
	// verify that the added count has the right value
	val, err := rc.readCount(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to read counter value after append:", err)
	}
	if val != 1 {
		t.Fatalf("read wrong counter value from disk after append. Expected 1, got %d", val)
	}
}

// TestRefCounterCreateAndApplyTransaction test that CreateAndApplyTransaction
// panics and restores the original in-memory structures on a failure to apply
// updates.
func TestRefCounterCreateAndApplyTransaction(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	numSec := 2 + fastrand.Uint64n(10)
	rc := testPrepareRefCounter(numSec, t)
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// add some valid updates
	updates := make([]writeaheadlog.Update, 0)
	u, err := rc.Append()
	if err != nil {
		t.Fatal("Failed to create an append update", err)
	}
	updates = append(updates, u)
	expectNumSec := numSec + 1
	u, err = rc.Increment(0)
	if err != nil {
		t.Fatal("Failed to create an increment update", err)
	}
	updates = append(updates, u)

	// add an invalid update that will cause an error
	u = writeaheadlog.Update{
		Name: "InvalidUpdate",
	}
	updates = append(updates, u)

	// add another valid update that will change the rc.numSectors, which change
	// must be reverted when we recover from the panic when applying the updates
	u, err = rc.DropSectors(1)
	if err != nil {
		t.Fatal("Failed to create a drop sectors update", err)
	}
	updates = append(updates, u)

	// make sure we panic because of the invalid update and that we restore the
	// count of sector number to the right value
	defer func() {
		// recover from a panic
		if r := recover(); r == nil {
			t.Fatal("Did not panic on an invalid update")
		}
		// make sure the number of sectors in memory is the expected one
		if rc.numSectors != expectNumSec {
			t.Fatal("Wrong number of sectors in memory after a panic")
		}
		if len(rc.newSectorCounts) != 0 {
			t.Fatal("Failed to drop the in-mem cache of new sector counts")
		}
	}()

	// apply the updates
	err = rc.CreateAndApplyTransaction(updates...)
	if err != nil {
		t.Fatal("Did not panic on invalid update, only returned an err:", err)
	} else {
		t.Fatal("Applied an invalid update without panicking or an error")
	}
}

// TestRefCounterDecrement tests that the Decrement method behaves correctly
func TestRefCounterDecrement(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Decrement
	secIdx := rc.numSectors - 2
	u, err := rc.Decrement(secIdx)
	if err != nil {
		t.Fatal("Failed to create an decrement update:", err)
	}

	// verify: we expect the value to have decreased the base from 1 to 0
	val, err := rc.readCount(secIdx)
	if err != nil {
		t.Fatal("Failed to read value after decrement:", err)
	}
	if val != 0 {
		t.Fatalf("read wrong value after decrement. Expected %d, got %d", 2, val)
	}

	// check behaviour on bad sector number
	_, err = rc.Decrement(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply decrement update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}
	// check the value on disk (the in-mem map is now gone)
	val, err = rc.readCount(secIdx)
	if err != nil {
		t.Fatal("Failed to read value after decrement:", err)
	}
	if val != 0 {
		t.Fatalf("read wrong value from disk after decrement. Expected 0, got %d", val)
	}
}

// TestRefCounterDelete tests that the Delete method behaves correctly
func TestRefCounterDelete(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// delete the ref counter
	u, err := rc.DeleteRefCounter()
	if err != nil {
		t.Fatal("Failed to create a delete update", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply a delete update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}

	// verify
	_, err = os.Stat(rc.filepath)
	if !os.IsNotExist(err) {
		t.Fatal("RefCounter deletion finished successfully but the file is still on disk", err)
	}
}

// TestRefCounterDropSectors tests that the DropSectors method behaves
// correctly and the file's size is properly adjusted
func TestRefCounterDropSectors(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	numSec := 2 + fastrand.Uint64n(10)
	rc := testPrepareRefCounter(numSec, t)
	stats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("RefCounter creation finished successfully but the file is not accessible:", err)
	}
	err = rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}
	var updates []writeaheadlog.Update
	// update both counters we intend to drop
	secIdx1 := rc.numSectors - 1
	secIdx2 := rc.numSectors - 2
	u, err := rc.Increment(secIdx1)
	if err != nil {
		t.Fatal("Failed to create truncate update:", err)
	}
	updates = append(updates, u)
	u, err = rc.Increment(secIdx2)
	if err != nil {
		t.Fatal("Failed to create truncate update:", err)
	}
	updates = append(updates, u)

	// check behaviour on bad sector number
	// (trying to drop more sectors than we have)
	_, err = rc.DropSectors(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// test DropSectors by dropping two counters
	u, err = rc.DropSectors(2)
	if err != nil {
		t.Fatal("Failed to create truncate update:", err)
	}
	updates = append(updates, u)
	expectNumSec := numSec - 2
	if rc.numSectors != expectNumSec {
		t.Fatalf("wrong number of counters after Truncate. Expected %d, got %d", expectNumSec, rc.numSectors)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(updates...)
	if err != nil {
		t.Fatal("Failed to apply truncate update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}

	//verify:  we expect the file size to have shrunk with 2*2 bytes
	endStats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("Failed to get file stats:", err)
	}
	expectSize := stats.Size() - 4
	actualSize := endStats.Size()
	if actualSize != expectSize {
		t.Fatalf("File size did not shrink as expected. Expected size: %d, actual size: %d", expectSize, actualSize)
	}
	// verify that we cannot read the values of the dropped counters
	_, err = rc.readCount(secIdx1)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}
	_, err = rc.readCount(secIdx2)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}
}

// TestRefCounterIncrement tests that the Increment method behaves correctly
func TestRefCounterIncrement(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Increment
	secIdx := rc.numSectors - 2
	u, err := rc.Increment(secIdx)
	if err != nil {
		t.Fatal("Failed to create an increment update:", err)
	}

	// verify: we expect the value to have increased from the base 1 to 2
	val, err := rc.readCount(secIdx)
	if err != nil {
		t.Fatal("Failed to read value after increment:", err)
	}
	if val != 2 {
		t.Fatalf("read wrong value after increment. Expected 2, got %d", val)
	}

	// check behaviour on bad sector number
	_, err = rc.Increment(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply increment update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}
	// check the value on disk (the in-mem map is now gone)
	val, err = rc.readCount(secIdx)
	if err != nil {
		t.Fatal("Failed to read value after increment:", err)
	}
	if val != 2 {
		t.Fatalf("read wrong value from disk after increment. Expected 2, got %d", val)
	}
}

// TestRefCounterLoad specifically tests refcounter's Load method
func TestRefCounterLoad(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter to load
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)

	// happy case
	_, err := LoadRefCounter(rc.filepath, testWAL)
	if err != nil {
		t.Fatal("Failed to load refcounter:", err)
	}

	// fails with os.ErrNotExist for a non-existent file
	_, err = LoadRefCounter("there-is-no-such-file.rc", testWAL)
	if !errors.IsOSNotExist(err) {
		t.Fatal("Expected os.ErrNotExist, got something else:", err)
	}
}

// TestRefCounterLoadInvalidHeader checks that loading a refcounters file with
// invalid header fails.
func TestRefCounterLoadInvalidHeader(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare
	cid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	d := build.TempDir(t.Name())
	err := os.MkdirAll(d, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(d, cid.String()+refCounterExtension)

	// Create a file that contains a corrupted header. This basically means
	// that the file is too short to have the entire header in there.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal("Failed to create test file:", err)
	}

	// The version number is 8 bytes. We'll only write 4.
	if _, err = f.Write(fastrand.Bytes(4)); err != nil {
		f.Close()
		t.Fatal("Failed to write to test file:", err)
	}
	f.Close()

	// Make sure we fail to load from that file and that we fail with the right
	// error
	_, err = LoadRefCounter(path, testWAL)
	if !errors.Contains(err, io.EOF) {
		t.Fatal(fmt.Sprintf("Should not be able to read file with bad header, expected `%s` error, got:", io.EOF.Error()), err)
	}
}

// TestRefCounterLoadInvalidVersion checks that loading a refcounters file
// with invalid version fails.
func TestRefCounterLoadInvalidVersion(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare
	cid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	d := build.TempDir(t.Name())
	err := os.MkdirAll(d, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(d, cid.String()+refCounterExtension)

	// create a file with a header that encodes a bad version number
	f, err := os.Create(path)
	if err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer f.Close()

	// The first 8 bytes are the version number. Write down an invalid one
	// followed 4 counters (another 8 bytes).
	_, err = f.Write(fastrand.Bytes(16))
	if err != nil {
		t.Fatal("Failed to write to test file:", err)
	}

	// ensure that we cannot load it and we return the correct error
	_, err = LoadRefCounter(path, testWAL)
	if !errors.Contains(err, ErrInvalidVersion) {
		t.Fatal(fmt.Sprintf("Should not be able to read file with wrong version, expected `%s` error, got:", ErrInvalidVersion.Error()), err)
	}
}

// TestRefCounterStartUpdate tests that the StartUpdate method respects the
// timeout limits set for it.
func TestRefCounterStartUpdate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// try to lock again with a timeout and see the timout trigger
	locked := make(chan error)
	timeout := time.After(time.Second)
	go func() {
		locked <- rc.StartUpdateWithTimeout(500 * time.Millisecond)
	}()
	select {
	case err = <-locked:
		if !errors.Contains(err, errTimeoutOnLock) {
			t.Fatal("Failed to timeout, expected errTimeoutOnLock, got:", err)
		}
	case <-timeout:
		t.Fatal("Failed to timeout, missed the deadline.")
	}

	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}
}

// TestRefCounterSwap tests that the Swap method results in correct values
func TestRefCounterSwap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	var updates []writeaheadlog.Update
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// increment one of the sectors, so we can tell the values apart
	u, err := rc.Increment(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to create increment update", err)
	}
	updates = append(updates, u)

	// test Swap
	us, err := rc.Swap(rc.numSectors-2, rc.numSectors-1)
	updates = append(updates, us...)
	if err != nil {
		t.Fatal("Failed to create swap update", err)
	}
	var v1, v2 uint16
	v1, err = rc.readCount(rc.numSectors - 2)
	if err != nil {
		t.Fatal("Failed to read value after swap", err)
	}
	v2, err = rc.readCount(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to read value after swap", err)
	}
	if v1 != 2 || v2 != 1 {
		t.Fatalf("read wrong value after swap. Expected %d and %d, got %d and %d", 2, 1, v1, v2)
	}

	// check behaviour on bad sector number
	_, err = rc.Swap(math.MaxInt64, 0)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the updates and check the values again
	err = rc.CreateAndApplyTransaction(updates...)
	if err != nil {
		t.Fatal("Failed to apply updates", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}
	// verify values on disk (the in-mem map is now gone)
	v1, err = rc.readCount(rc.numSectors - 2)
	if err != nil {
		t.Fatal("Failed to read value from disk after swap", err)
	}
	v2, err = rc.readCount(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to read value from disk after swap", err)
	}
	if v1 != 2 || v2 != 1 {
		t.Fatalf("read wrong value from disk after swap. Expected %d and %d, got %d and %d", 2, 1, v1, v2)
	}
}

// TestRefCounterUpdateApplied tests that the UpdateApplied method cleans up
// after itself
func TestRefCounterUpdateApplied(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	var updates []writeaheadlog.Update
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// generate some update
	secIdx := rc.numSectors - 1
	u, err := rc.Increment(secIdx)
	if err != nil {
		t.Fatal("Failed to create increment update", err)
	}
	updates = append(updates, u)
	// verify that the override map reflects the update
	if _, ok := rc.newSectorCounts[secIdx]; !ok {
		t.Fatal("Failed to update the in-mem override map.")
	}

	// apply the updates and check the values again
	err = rc.CreateAndApplyTransaction(updates...)
	if err != nil {
		t.Fatal("Failed to apply updates", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}
	// verify that the in-mem override map is now cleaned up
	if len(rc.newSectorCounts) != 0 {
		t.Fatalf("updateApplied failed to clean up the newSectorCounts. Expected len 0, got %d", len(rc.newSectorCounts))
	}
}

// TestRefCounterUpdateSessionConstraints ensures that StartUpdate() and UpdateApplied()
// enforce all applicable restrictions to update creation and execution
func TestRefCounterUpdateSessionConstraints(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)

	var u writeaheadlog.Update
	// make sure we cannot create updates outside of an update session
	_, err1 := rc.Append()
	_, err2 := rc.Decrement(1)
	_, err3 := rc.DeleteRefCounter()
	_, err4 := rc.DropSectors(1)
	_, err5 := rc.Increment(1)
	_, err6 := rc.Swap(1, 2)
	err7 := rc.CreateAndApplyTransaction(u)
	for i, err := range []error{err1, err2, err3, err4, err5, err6, err7} {
		if !errors.Contains(err, ErrUpdateWithoutUpdateSession) {
			t.Fatalf("err%v: expected %v but was %v", i+1, ErrUpdateWithoutUpdateSession, err)
		}
	}

	// start an update session
	err := rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}
	// delete the ref counter
	u, err = rc.DeleteRefCounter()
	if err != nil {
		t.Fatal("Failed to create a delete update", err)
	}
	// make sure we cannot create any updates after a deletion has been triggered
	_, err1 = rc.Append()
	_, err2 = rc.Decrement(1)
	_, err3 = rc.DeleteRefCounter()
	_, err4 = rc.DropSectors(1)
	_, err5 = rc.Increment(1)
	_, err6 = rc.Swap(1, 2)
	for i, err := range []error{err1, err2, err3, err4, err5, err6} {
		if !errors.Contains(err, ErrUpdateAfterDelete) {
			t.Fatalf("err%v: expected %v but was %v", i+1, ErrUpdateAfterDelete, err)
		}
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply a delete update:", err)
	}
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to finish the update session:", err)
	}

	// make sure we cannot start an update session on a deleted counter
	if err = rc.StartUpdateWithTimeout(-1); err != ErrUpdateAfterDelete {
		t.Fatal("Failed to prevent an update creation after a deletion", err)
	}
}

// TestRefCounterWALFunctions tests RefCounter's functions for creating and
// reading WAL updates
func TestRefCounterWALFunctions(t *testing.T) {
	t.Parallel()

	// test creating and reading updates
	wpath := "test/writtenPath"
	wsec := uint64(2)
	wval := uint16(12)
	u := createWriteAtUpdate(wpath, wsec, wval)
	rpath, rsec, rval, err := readWriteAtUpdate(u)
	if err != nil {
		t.Fatal("Failed to read writeAt update:", err)
	}
	if wpath != rpath || wsec != rsec || wval != rval {
		t.Fatalf("wrong values read from WriteAt update. Expected %s, %d, %d, found %s, %d, %d", wpath, wsec, wval, rpath, rsec, rval)
	}

	u = createTruncateUpdate(wpath, wsec)
	rpath, rsec, err = readTruncateUpdate(u)
	if err != nil {
		t.Fatal("Failed to read a truncate update:", err)
	}
	if wpath != rpath || wsec != rsec {
		t.Fatalf("wrong values read from Truncate update. Expected %s, %d found %s, %d", wpath, wsec, rpath, rsec)
	}
}

// TestRefCounterNumSectorsUnderflow tests for and guards against an NDF that
// can happen in various methods when numSectors is zero and we check the sector
// index to be read against numSectors-1.
func TestRefCounterNumSectorsUnderflow(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter with zero sectors for the tests
	rc := testPrepareRefCounter(0, t)

	// try to read the nonexistent sector with index 0
	_, err := rc.readCount(0)
	// when checking if the sector we want to read is valid we compare it to
	// numSectors. If we do it by comparing `secNum > numSectors - 1` we will
	// hit an underflow which will result in the check passing and us getting
	// an EOF error instead of the correct ErrInvalidSectorNumber
	if errors.Contains(err, io.EOF) {
		t.Fatal("Unexpected EOF error instead of ErrInvalidSectorNumber. Underflow!")
	}
	// we should get an ErrInvalidSectorNumber
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	err = rc.StartUpdateWithTimeout(-1)
	if err != nil {
		t.Fatal("Failed to initiate an update session:", err)
	}

	// check for the same underflow during Decrement
	_, err = rc.Decrement(0)
	if errors.Contains(err, io.EOF) {
		t.Fatal("Unexpected EOF error instead of ErrInvalidSectorNumber. Underflow!")
	}
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// check for the same underflow during Increment
	_, err = rc.Increment(0)
	if errors.Contains(err, io.EOF) {
		t.Fatal("Unexpected EOF error instead of ErrInvalidSectorNumber. Underflow!")
	}
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// check for the same underflow during Swap
	_, err1 := rc.Swap(0, 1)
	_, err2 := rc.Swap(1, 0)
	err = errors.Compose(err1, err2)
	if errors.Contains(err, io.EOF) {
		t.Fatal("Unexpected EOF error instead of ErrInvalidSectorNumber. Underflow!")
	}
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// cleanup the update session
	err = rc.UpdateApplied()
	if err != nil {
		t.Fatal("Failed to wrap up an empty update session:", err)
	}
}

// newTestWal is a helper method to create a WAL for testing.
func newTestWAL() (*writeaheadlog.WAL, string) {
	// Create the wal.
	wd := filepath.Join(os.TempDir(), "rc-wals")
	if err := os.MkdirAll(wd, modules.DefaultDirPerm); err != nil {
		panic(err)
	}
	walFilePath := filepath.Join(wd, hex.EncodeToString(fastrand.Bytes(8)))
	_, wal, err := writeaheadlog.New(walFilePath)
	if err != nil {
		panic(err)
	}
	return wal, walFilePath
}

// testPrepareRefCounter is a helper that creates a refcounter and fails the
// test if that is not successful
func testPrepareRefCounter(numSec uint64, t *testing.T) *RefCounter {
	tcid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	td := build.TempDir(t.Name())
	err := os.MkdirAll(td, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(td, tcid.String()+refCounterExtension)
	// create a ref counter
	rc, err := NewRefCounter(path, numSec, testWAL)
	if err != nil {
		t.Fatal("Failed to create a reference counter:", err)
	}
	return rc
}

// writeVal is a helper method that writes a certain counter value to disk. This
// method does not do any validations or checks, the caller must make certain
// that the input parameters are valid.
func writeVal(path string, secIdx uint64, val uint16) error {
	f, err := os.OpenFile(path, os.O_RDWR, modules.DefaultFilePerm)
	if err != nil {
		return errors.AddContext(err, "failed to open refcounter file")
	}
	defer f.Close()
	var b u16
	binary.LittleEndian.PutUint16(b[:], val)
	if _, err = f.WriteAt(b[:], int64(offset(secIdx))); err != nil {
		return errors.AddContext(err, "failed to write to refcounter file")
	}
	return nil
}
