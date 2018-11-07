package host

// TODO: seems like there would be problems with the negotiation protocols if
// the renter tried something like 'form' or 'renew' but then the connections
// dropped after the host completed the transaction but before the host was
// able to send the host signatures for the transaction.
//
// Especially on a renew, the host choosing to hold the renter signatures
// hostage could be a pretty significant problem, and would require the renter
// to attempt a double-spend to either force the transaction onto the
// blockchain or to make sure that the host cannot abscond with the funds
// without commitment.
//
// Incentive for the host to do such a thing is pretty low - they will still
// have to keep all the files following a renew in order to get the money.

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/coreos/bbolt"
	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/fastrand"
)

// rpcSettingsDeprecated is a specifier for a deprecated settings request.
var rpcSettingsDeprecated = types.Specifier{'S', 'e', 't', 't', 'i', 'n', 'g', 's'}

// threadedUpdateHostname periodically runs 'managedLearnHostname', which
// checks if the host's hostname has changed, and makes an updated host
// announcement if so.
func (h *Host) threadedUpdateHostname(closeChan chan struct{}) {
	defer close(closeChan)
	for {
		h.managedLearnHostname()
		// Wait 30 minutes to check again. If the hostname is changing
		// regularly (more than once a week), we want the host to be able to be
		// seen as having 95% uptime. Every minute that the announcement is
		// pointing to the wrong address is a minute of perceived downtime to
		// the renters.
		select {
		case <-h.tg.StopChan():
			return
		case <-time.After(time.Minute * 30):
			continue
		}
	}
}

// threadedTrackWorkingStatus periodically checks if the host is working,
// where working is defined as having received 3 settings calls in the past 15
// minutes.
func (h *Host) threadedTrackWorkingStatus(closeChan chan struct{}) {
	defer close(closeChan)

	// Before entering the longer loop, try a greedy, faster attempt to verify
	// that the host is working.
	prevSettingsCalls := atomic.LoadUint64(&h.atomicSettingsCalls)
	select {
	case <-h.tg.StopChan():
		return
	case <-time.After(workingStatusFirstCheck):
	}
	settingsCalls := atomic.LoadUint64(&h.atomicSettingsCalls)

	// sanity check
	if prevSettingsCalls > settingsCalls {
		build.Severe("the host's settings calls decremented")
	}

	h.mu.Lock()
	if settingsCalls-prevSettingsCalls >= workingStatusThreshold {
		h.workingStatus = modules.HostWorkingStatusWorking
	}
	// First check is quick, don't set to 'not working' if host has not been
	// contacted enough times.
	h.mu.Unlock()

	for {
		prevSettingsCalls = atomic.LoadUint64(&h.atomicSettingsCalls)
		select {
		case <-h.tg.StopChan():
			return
		case <-time.After(workingStatusFrequency):
		}
		settingsCalls = atomic.LoadUint64(&h.atomicSettingsCalls)

		// sanity check
		if prevSettingsCalls > settingsCalls {
			build.Severe("the host's settings calls decremented")
			continue
		}

		h.mu.Lock()
		if settingsCalls-prevSettingsCalls >= workingStatusThreshold {
			h.workingStatus = modules.HostWorkingStatusWorking
		} else {
			h.workingStatus = modules.HostWorkingStatusNotWorking
		}
		h.mu.Unlock()
	}
}

// threadedTrackConnectabilityStatus periodically checks if the host is
// connectable at its netaddress.
func (h *Host) threadedTrackConnectabilityStatus(closeChan chan struct{}) {
	defer close(closeChan)

	// Wait briefly before checking the first time. This gives time for any port
	// forwarding to complete.
	select {
	case <-h.tg.StopChan():
		return
	case <-time.After(connectabilityCheckFirstWait):
	}

	for {
		h.mu.RLock()
		autoAddr := h.autoAddress
		userAddr := h.settings.NetAddress
		h.mu.RUnlock()

		activeAddr := autoAddr
		if userAddr != "" {
			activeAddr = userAddr
		}

		dialer := &net.Dialer{
			Cancel:  h.tg.StopChan(),
			Timeout: connectabilityCheckTimeout,
		}
		conn, err := dialer.Dial("tcp", string(activeAddr))

		var status modules.HostConnectabilityStatus
		if err != nil {
			status = modules.HostConnectabilityStatusNotConnectable
		} else {
			conn.Close()
			status = modules.HostConnectabilityStatusConnectable
		}
		h.mu.Lock()
		h.connectabilityStatus = status
		h.mu.Unlock()

		select {
		case <-h.tg.StopChan():
			return
		case <-time.After(connectabilityCheckFrequency):
		}
	}
}

// initNetworking performs actions like port forwarding, and gets the
// host established on the network.
func (h *Host) initNetworking(address string) (err error) {
	// Create the listener and setup the close procedures.
	h.listener, err = h.dependencies.Listen("tcp", address)
	if err != nil {
		return err
	}
	// Automatically close the listener when h.tg.Stop() is called.
	threadedListenerClosedChan := make(chan struct{})
	h.tg.OnStop(func() {
		err := h.listener.Close()
		if err != nil {
			h.log.Println("WARN: closing the listener failed:", err)
		}

		// Wait until the threadedListener has returned to continue shutdown.
		<-threadedListenerClosedChan
	})

	// Set the initial working state of the host
	h.workingStatus = modules.HostWorkingStatusChecking

	// Set the initial connectability state of the host
	h.connectabilityStatus = modules.HostConnectabilityStatusChecking

	// Set the port.
	_, port, err := net.SplitHostPort(h.listener.Addr().String())
	if err != nil {
		return err
	}
	h.port = port
	if build.Release == "testing" {
		// Set the autoAddress to localhost for testing builds only.
		h.autoAddress = modules.NetAddress(net.JoinHostPort("localhost", h.port))
	}

	// Non-blocking, perform port forwarding and create the hostname discovery
	// thread.
	go func() {
		// Add this function to the threadgroup, so that the logger will not
		// disappear before port closing can be registered to the threadgrourp
		// OnStop functions.
		err := h.tg.Add()
		if err != nil {
			// If this goroutine is not run before shutdown starts, this
			// codeblock is reachable.
			return
		}
		defer h.tg.Done()

		err = h.g.ForwardPort(port)
		if err != nil {
			h.log.Println("ERROR: failed to forward port:", err)
		}

		threadedUpdateHostnameClosedChan := make(chan struct{})
		go h.threadedUpdateHostname(threadedUpdateHostnameClosedChan)
		h.tg.OnStop(func() {
			<-threadedUpdateHostnameClosedChan
		})

		threadedTrackWorkingStatusClosedChan := make(chan struct{})
		go h.threadedTrackWorkingStatus(threadedTrackWorkingStatusClosedChan)
		h.tg.OnStop(func() {
			<-threadedTrackWorkingStatusClosedChan
		})

		threadedTrackConnectabilityStatusClosedChan := make(chan struct{})
		go h.threadedTrackConnectabilityStatus(threadedTrackConnectabilityStatusClosedChan)
		h.tg.OnStop(func() {
			<-threadedTrackConnectabilityStatusClosedChan
		})
	}()

	// Launch the listener.
	go h.threadedListen(threadedListenerClosedChan)
	return nil
}

// threadedHandleConn handles an incoming connection to the host, typically an
// RPC.
func (h *Host) threadedHandleConn(conn net.Conn) {
	err := h.tg.Add()
	if err != nil {
		return
	}
	defer h.tg.Done()

	// Close the conn on host.Close or when the method terminates, whichever comes
	// first.
	connCloseChan := make(chan struct{})
	defer close(connCloseChan)
	go func() {
		select {
		case <-h.tg.StopChan():
		case <-connCloseChan:
		}
		conn.Close()
	}()

	// Set an initial duration that is generous, but finite. RPCs can extend
	// this if desired.
	err = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if err != nil {
		h.log.Println("WARN: could not set deadline on connection:", err)
		return
	}

	// Read a specifier indicating which action is being called.
	var id types.Specifier
	if err := encoding.ReadObject(conn, &id, 16); err != nil {
		atomic.AddUint64(&h.atomicUnrecognizedCalls, 1)
		h.log.Debugf("WARN: incoming conn %v was malformed: %v", conn.RemoteAddr(), err)
		return
	}

	switch id {
	// new RPCs: enter an infinite request/response loop
	case modules.RPCLoopEnter:
		err = extendErr("incoming RPCLoopEnter failed: ", h.managedRPCLoop(conn))
	// old RPCs: handle a single request/response
	case modules.RPCDownload:
		atomic.AddUint64(&h.atomicDownloadCalls, 1)
		err = extendErr("incoming RPCDownload failed: ", h.managedRPCDownload(conn))
	case modules.RPCRenewContract:
		atomic.AddUint64(&h.atomicRenewCalls, 1)
		err = extendErr("incoming RPCRenewContract failed: ", h.managedRPCRenewContract(conn))
	case modules.RPCFormContract:
		atomic.AddUint64(&h.atomicFormContractCalls, 1)
		err = extendErr("incoming RPCFormContract failed: ", h.managedRPCFormContract(conn))
	case modules.RPCReviseContract:
		atomic.AddUint64(&h.atomicReviseCalls, 1)
		err = extendErr("incoming RPCReviseContract failed: ", h.managedRPCReviseContract(conn))
	case modules.RPCSettings:
		atomic.AddUint64(&h.atomicSettingsCalls, 1)
		err = extendErr("incoming RPCSettings failed: ", h.managedRPCSettings(conn))
	case rpcSettingsDeprecated:
		h.log.Debugln("Received deprecated settings call")
	default:
		h.log.Debugf("WARN: incoming conn %v requested unknown RPC \"%v\"", conn.RemoteAddr(), id)
		atomic.AddUint64(&h.atomicUnrecognizedCalls, 1)
	}
	if err != nil {
		atomic.AddUint64(&h.atomicErroredCalls, 1)
		err = extendErr("error with "+conn.RemoteAddr().String()+": ", err)
		h.managedLogError(err)
	}
}

// managedRPCLoop reads new RPCs from the renter, each consisting of a single
// request and response. The loop terminates when the an RPC encounters an
// error or the renter sends modules.RPCLoopExit.
func (h *Host) managedRPCLoop(conn net.Conn) error {
	// perform initial handshake
	conn.SetDeadline(time.Now().Add(rpcRequestInterval))
	var req modules.LoopHandshakeRequest
	if err := encoding.NewDecoder(conn).Decode(&req); err != nil {
		modules.WriteRPCResponse(conn, nil, err)
		return err
	}

	// check handshake version and ciphers
	if req.Version != 1 {
		err := errors.New("protocol version not supported")
		modules.WriteRPCResponse(conn, nil, err)
		return err
	}
	var supportsPlaintext bool
	for _, c := range req.Ciphers {
		if c == modules.CipherPlaintext {
			supportsPlaintext = true
		}
	}
	if !supportsPlaintext {
		err := errors.New("no supported ciphers")
		modules.WriteRPCResponse(conn, nil, err)
		return err
	}

	// send handshake response
	var challenge [16]byte
	fastrand.Read(challenge[:])
	resp := modules.LoopHandshakeResponse{
		Cipher:    modules.CipherPlaintext,
		Challenge: challenge,
	}
	if err := modules.WriteRPCResponse(conn, resp, nil); err != nil {
		return err
	}

	// read challenge response
	var cresp modules.LoopChallengeResponse
	if err := encoding.NewDecoder(conn).Decode(&req); err != nil {
		modules.WriteRPCResponse(conn, nil, err)
		return err
	}

	// if a contract was supplied, look it up, verify the challenge response,
	// and lock the storage obligation
	var so storageObligation
	if req.ContractID != (types.FileContractID{}) {
		// NOTE: if we encounter an error here, we send it to the renter and
		// close the connection immediately. From the renter's perspective,
		// this error may arrive either before or after sending their first
		// RPC request.

		// look up the renter's public key
		var err error
		h.mu.RLock()
		err = h.db.View(func(tx *bolt.Tx) error {
			so, err = getStorageObligation(tx, req.ContractID)
			return err
		})
		h.mu.RUnlock()
		if err != nil {
			modules.WriteRPCResponse(conn, nil, errors.New("no record of that contract"))
			return extendErr("could not lock contract "+req.ContractID.String()+": ", err)
		}

		// verify the challenge response
		rev := so.RevisionTransactionSet[len(so.RevisionTransactionSet)-1].FileContractRevisions[0]
		hash := crypto.HashAll(modules.RPCChallengePrefix, challenge)
		var renterPK crypto.PublicKey
		var renterSig crypto.Signature
		copy(renterPK[:], rev.UnlockConditions.PublicKeys[0].Key)
		copy(renterSig[:], cresp.Signature)
		if crypto.VerifyHash(hash, renterPK, renterSig) != nil {
			err := errors.New("challenge signature is invalid")
			modules.WriteRPCResponse(conn, nil, err)
			return err
		}

		// lock the storage obligation until the end of the RPC loop
		if err := h.managedTryLockStorageObligation(req.ContractID); err != nil {
			modules.WriteRPCResponse(conn, nil, err)
			return extendErr("could not lock contract "+req.ContractID.String()+": ", err)
		}
		defer h.managedUnlockStorageObligation(req.ContractID)
	}

	// enter RPC loop
	for {
		conn.SetDeadline(time.Now().Add(rpcRequestInterval))

		var id types.Specifier
		if _, err := io.ReadFull(conn, id[:]); err != nil {
			h.log.Debugf("WARN: renter sent invalid RPC ID: %v", id)
			return errors.New("invalid RPC ID " + id.String())
		}

		var err error
		switch id {
		case modules.RPCLoopSettings:
			err = extendErr("incoming RPCLoopSettings failed: ", h.managedRPCLoopSettings(conn))
		case modules.RPCLoopRecentRevision:
			err = extendErr("incoming RPCLoopRecentRevision failed: ", h.managedRPCLoopRecentRevision(conn, &so, challenge))
		case modules.RPCLoopUpload:
			err = extendErr("incoming RPCLoopUpload failed: ", h.managedRPCLoopUpload(conn, &so))
		case modules.RPCLoopDownload:
			err = extendErr("incoming RPCLoopDownload failed: ", h.managedRPCLoopDownload(conn, &so))
		case modules.RPCLoopSectorRoots:
			err = extendErr("incoming RPCLoopSectorRoots failed: ", h.managedRPCLoopSectorRoots(conn, &so))
		case modules.RPCLoopExit:
			return nil
		default:
			return errors.New("invalid or unknown RPC ID: " + id.String())
		}
		if err != nil {
			return err
		}
	}
}

// threadedListen listens for incoming RPCs and spawns an appropriate handler for each.
func (h *Host) threadedListen(closeChan chan struct{}) {
	defer close(closeChan)

	// Receive connections until an error is returned by the listener. When an
	// error is returned, there will be no more calls to receive.
	for {
		// Block until there is a connection to handle.
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}

		go h.threadedHandleConn(conn)

		// Soft-sleep to ratelimit the number of incoming connections.
		select {
		case <-h.tg.StopChan():
		case <-time.After(rpcRatelimit):
		}
	}
}

// NetAddress returns the address at which the host can be reached.
func (h *Host) NetAddress() modules.NetAddress {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.settings.NetAddress != "" {
		return h.settings.NetAddress
	}
	return h.autoAddress
}

// NetworkMetrics returns information about the types of rpc calls that have
// been made to the host.
func (h *Host) NetworkMetrics() modules.HostNetworkMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return modules.HostNetworkMetrics{
		DownloadCalls:     atomic.LoadUint64(&h.atomicDownloadCalls),
		ErrorCalls:        atomic.LoadUint64(&h.atomicErroredCalls),
		FormContractCalls: atomic.LoadUint64(&h.atomicFormContractCalls),
		RenewCalls:        atomic.LoadUint64(&h.atomicRenewCalls),
		ReviseCalls:       atomic.LoadUint64(&h.atomicReviseCalls),
		SettingsCalls:     atomic.LoadUint64(&h.atomicSettingsCalls),
		UnrecognizedCalls: atomic.LoadUint64(&h.atomicUnrecognizedCalls),
	}
}
