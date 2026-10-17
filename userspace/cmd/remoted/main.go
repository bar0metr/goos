//go:build goos_userspace

// Command remoted owns the listener; workers receive only ciphertext streams.
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"goos/internal/capability"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/nativefiles"
	"goos/userspace/remoteapi"
	"goos/userspace/remotepolicy"
	"goos/userspace/remoteservice"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpdispatch"
	"goos/userspace/utility"
	userwait "goos/userspace/wait"
	"strings"
)

const budget = 5 * clock.TicksPerSecond

var control, configChannel, vfsChannel, tcpChannel handle.ID
var dispatch tcpdispatch.Session
var state remoteservice.State
var identities remoteservice.Identities
var configClient config.Client
var files nativefiles.Files
var bulk utility.Bulk
var sequence, accept uint64
var acceptListener uint32
var pending, authenticated int
var admins [2]handle.ID
var workers [remoteapi.MaxWorkers]worker
var readBuffer [tcpapi.SlotSize]byte
var nextWorker int
var pumpingRPC bool
var durableSnapshot remoteservice.Snapshot
var durableHostPath, durableKeysPath [64]byte
var durableFingerprint [64]byte
var durableUsername [remoteapi.MaxUsername]byte
var durableKEX = []string{remotepolicy.KEXAlgorithm}
var durableCipher = []string{remotepolicy.CipherAlgorithm}
var durableKeyAlgorithm = []string{remotepolicy.KeyAlgorithm}

type worker struct {
	signer                                                handle.ID
	generation, signSequence                              uint64
	identity                                              int
	identityHeld                                          bool
	endpoint                                              uint32
	token                                                 uint64
	input, output                                         handle.ID
	read, write, close                                    uint64
	shutdown, drainDeadline                               uint64
	outputEOF, inputGone, tcpEOF, shutdownDone, closeDone bool
	written                                               int
	toWorker, toTCP                                       remoteservice.Queue
}

type hostFiles struct {
	*nativefiles.Files
	locked bool
}

func (f *hostFiles) Lock() error {
	if f.locked {
		return errors.New("host key writer busy")
	}
	f.locked = true
	return nil
}
func (f *hostFiles) Unlock() error { f.locked = false; return nil }

// PID 1 creates the exact host-key parents before issuing the confined grant.
// This adapter must not ask VFS for broader ancestor authority.
func (f *hostFiles) Mkdir(string) error { return nil }

var hostAdapter hostFiles

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { sys.Exit(1) })
	arena.InitPackages()
	data, ok := startup.Read()
	if !ok {
		sys.Exit(1)
	}
	logHandle, ok := data.Handle("log")
	if !ok {
		sys.Exit(1)
	}
	syslog.Init(logHandle, "remoted")
	control, ok = data.Handle("control")
	if !ok {
		sys.Exit(1)
	}
	configChannel, ok = data.Handle("config")
	if !ok {
		sys.Exit(1)
	}
	vfsChannel, ok = data.Handle("vfs")
	if !ok {
		sys.Exit(1)
	}
	utility.BindVFS(vfsChannel)
	tcpChannel, ok = data.Handle("tcp")
	if !ok {
		sys.Exit(1)
	}
	tx, ok := data.Handle("tcp-tx")
	if !ok {
		sys.Exit(1)
	}
	rx, ok := data.Handle("tcp-rx")
	if !ok {
		sys.Exit(1)
	}
	txBase, txSize, status := sys.MemoryMapAnywhere(tx, true)
	if status != kcall.StatusOK || txSize != tcpapi.TXSlots*tcpapi.SlotSize {
		sys.Exit(1)
	}
	rxBase, rxSize, status := sys.MemoryMapAnywhere(rx, false)
	if status != kcall.StatusOK || rxSize != tcpapi.RXSlots*tcpapi.SlotSize {
		sys.Exit(1)
	}
	if !dispatch.Init(sys.Window(txBase, txSize), sys.Window(rxBase, rxSize), func(message *ipc.Message) bool { return remoteservice.Send(tcpChannel, message) }) {
		sys.Exit(1)
	}
	bulk, ok = utility.NewBulk(utility.BulkSize)
	if !ok {
		sys.Exit(1)
	}
	files.Bulk = &bulk
	hostAdapter.Files = &files
	if err := reload(); err != nil {
		syslog.Error("remote startup rejected", syslog.Str("reason", err.Error()))
		state.Reject(err.Error())
	}
	syslog.Info("remote service ready")
	for {
		serviceControl()
		serviceTCP()
		serviceAdmins()
		serviceSigners()
		advance()
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		count := 0
		add := func(id handle.ID, events kcall.WaitEvent) {
			if id != 0 {
				interests[count] = sys.Interest{Handle: id, Events: events | kcall.WaitPeerClosed}
				count++
			}
		}
		add(control, kcall.WaitReadable)
		add(tcpChannel, kcall.WaitReadable)
		for _, channel := range admins {
			add(channel, kcall.WaitReadable)
		}
		for i := range workers {
			w := &workers[i]
			if w.close != 0 {
				continue
			}
			add(w.signer, kcall.WaitReadable)
			if !w.outputEOF && w.toTCP.Free() >= 256 {
				add(w.output, kcall.WaitReadable)
			}
			if !w.inputGone && !w.outputEOF && w.toWorker.Len() != 0 {
				add(w.input, kcall.WaitWritable)
			}
		}
		_, status := userwait.Any(&interests, count, clock.TicksPerSecond)
		if status != kcall.StatusOK && status != kcall.StatusTimeout {
			syslog.Error("remote wait failed", syslog.Uint("status", uint64(status)), syslog.Uint("interests", uint64(count)))
			sys.Exit(1)
		}
	}
}

type configTransport struct{}

func (configTransport) Send(message *ipc.Message) bool {
	return remoteservice.Send(configChannel, message)
}
func (configTransport) Receive(ticks uint64, out *ipc.Message) kcall.Status {
	channel := configChannel
	deadline := remoteservice.Now() + ticks
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	for remoteservice.Now() < deadline {
		serviceTCP()
		serviceSigners()
		advance()
		serviceControl()
		status := remoteservice.Receive(channel, 0, out)
		if status != kcall.StatusNotFound {
			return status
		}
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		interests[0] = sys.Interest{Handle: channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = sys.Interest{Handle: tcpChannel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		_, status = sys.WaitAny(&interests, 2, 1)
		if status != kcall.StatusOK && status != kcall.StatusTimeout {
			return status
		}
	}
	return kcall.StatusTimeout
}
func (configTransport) Now() uint64 { return remoteservice.Now() }

func revision(deadline uint64) (uint64, error) {
	request, ok := config.EncodeRequest(config.Request{Operation: config.OperationRevision}, 1)
	var reply ipc.Message
	if !ok || configClient.Exchange(configTransport{}, &request, deadline, &reply) != config.OutcomeOK {
		return 0, errors.New("remote configuration revision unavailable")
	}
	value, valid := config.DecodeReply(reply)
	if !valid || value.Status != config.StatusOK || value.Operation != config.OperationRevision || value.Value.Kind != config.KindUint {
		return 0, errors.New("invalid remote configuration revision")
	}
	return value.Value.Uint, nil
}
func reload() error {
	if remoteUpdateBusy {
		return errors.New("remote update busy")
	}
	remoteUpdateBusy = true
	defer func() { remoteUpdateBusy = false }()
	return reloadCandidate(nil, 0, "", &files)
}
func reloadCandidate(expected []byte, expectedRevision uint64, expectedPath string, keyReader interface {
	Read(string, int) ([]byte, error)
}) error {
	deadline := remoteservice.Now() + budget
	before, err := revision(deadline)
	if err != nil {
		return err
	}
	if expectedRevision != 0 && before != expectedRevision {
		return errors.New("remote configuration ownership changed")
	}
	policy, err := remotepolicy.Load(func(key string) (config.Value, config.Status) {
		var message ipc.Message
		value, status, outcome := configClient.ExchangeGet(configTransport{}, []byte(key), deadline, &message)
		if outcome != config.OutcomeOK {
			return config.Value{}, config.StatusResourceExhausted
		}
		return value, status
	})
	if err != nil {
		return err
	}
	if !strings.HasPrefix(policy.HostKey, "/state/") {
		return errors.New("invalid remote setting: host-key must be under /state/")
	}
	after, err := revision(deadline)
	if err != nil || after != before {
		return errors.New("remote configuration changed during reload")
	}
	oldVFS, oldConfig := vfsChannel, configChannel
	var newVFS, newConfig handle.ID
	authorityChanged := false
	if expectedRevision == 0 {
		var authority ipc.Message
		request := remoteapi.WorkerRequest{Operation: remoteapi.WorkerAuthority, HostPath: []byte(policy.HostKey), AuthorizedKeys: []byte(policy.AuthorizedKeys)}
		if !workerCall(&request, nil, &authority) {
			return errors.New("remote file authority unavailable")
		}
		newVFS, newConfig = authority.Handles[0], authority.Handles[1]
		vfsChannel, configChannel = newVFS, newConfig
		utility.BindVFS(newVFS)
		authorityChanged = true
	}
	committed := false
	defer func() {
		if !authorityChanged {
			return
		}
		if committed {
			sys.HandleClose(oldVFS)
			sys.HandleClose(oldConfig)
		} else {
			utility.BindVFS(oldVFS)
			vfsChannel, configChannel = oldVFS, oldConfig
			sys.HandleClose(newVFS)
			sys.HandleClose(newConfig)
		}
	}()
	var host *remotepolicy.HostKey
	if expectedRevision != 0 {
		host, err = keyReloadHost(policy)
	} else {
		host, err = (&remotepolicy.HostStore{Files: &hostAdapter}).Load(policy.HostKey)
	}
	if err != nil {
		return err
	}
	defer clear(host.Private)
	keyData, err := keyReader.Read(policy.AuthorizedKeys, remotepolicy.MaxAuthorizedKeysBytes)
	if errors.Is(err, remotepolicy.ErrNotFound) && !policy.Enabled {
		keyData = nil
		err = nil
	}
	if err != nil {
		return err
	}
	if expectedRevision != 0 && (policy.AuthorizedKeys != expectedPath || !bytes.Equal(keyData, expected)) {
		return errors.New("authorized key candidate ownership changed")
	}
	parsed, err := remotepolicy.ParseAuthorizedKeys(keyData)
	if err != nil {
		return err
	}
	keys := make([]ed25519.PublicKey, parsed.Len())
	for i := range keys {
		keys[i], _ = parsed.At(i)
	}
	next, err := state.Prepare(policy, host, keys)
	if err != nil {
		return err
	}
	identity, ok := identities.Current()
	if expectedRevision == 0 {
		identity, ok = identities.Reserve(host.Private)
	}
	if !ok {
		return errors.New("remote identity capacity exhausted")
	}
	defer identities.Discard(identity)
	old := state.Current
	replacement := uint32(0)
	if policy.Enabled {
		if old != nil && remoteservice.SameBind(old.Policy, policy) {
			next.Listener = old.Listener
		} else {
			opened, ok := tcpCall(tcpapi.Request{Opcode: tcpapi.OpOpen})
			if !ok {
				return errors.New("remote listener allocation failed")
			}
			replacement = opened.Endpoint
			bound, ok := tcpCall(tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: replacement, Address: policy.ListenAddress, Port: policy.Port})
			if !ok || bound.Port != policy.Port {
				closeEndpoint(replacement)
				return errors.New("remote listener bind failed")
			}
			if _, ok := tcpCall(tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: replacement, Backlog: 2}); !ok {
				closeEndpoint(replacement)
				return errors.New("remote listener listen failed")
			}
			next.Listener = replacement
		}
	}
	final, err := revision(remoteservice.Now() + budget)
	if err != nil || final != before {
		if replacement != 0 {
			closeEndpoint(replacement)
		}
		return errors.New("remote configuration changed before commit")
	}
	if expectedRevision != 0 {
		persisted, check := keyReader.Read(expectedPath, remotepolicy.MaxAuthorizedKeysBytes)
		checkedRevision, revisionErr := revision(remoteservice.Now() + budget)
		if check != nil || revisionErr != nil || checkedRevision != before || !bytes.Equal(persisted, expected) {
			if replacement != 0 {
				closeEndpoint(replacement)
			}
			return errors.New("authorized key candidate changed before commit")
		}
	}
	if !state.Commit(next) {
		if replacement != 0 {
			closeEndpoint(replacement)
		}
		return errors.New("remote policy commit failed")
	}
	if !identities.Commit(identity) {
		sys.Exit(1)
	}
	oldListener := uint32(0)
	if old != nil {
		oldListener = old.Listener
	}
	durableSnapshot = *next
	durableSnapshot.Policy.User = sys.String(durableUsername[:copy(durableUsername[:], []byte(next.Policy.User))])
	durableSnapshot.Policy.HostKey = sys.String(durableHostPath[:copy(durableHostPath[:], []byte(next.Policy.HostKey))])
	durableSnapshot.Policy.AuthorizedKeys = sys.String(durableKeysPath[:copy(durableKeysPath[:], []byte(next.Policy.AuthorizedKeys))])
	durableSnapshot.Fingerprint = sys.String(durableFingerprint[:copy(durableFingerprint[:], []byte(next.Fingerprint))])
	durableSnapshot.Policy.KEX, durableSnapshot.Policy.Ciphers = durableKEX, durableCipher
	durableSnapshot.Policy.HostKeyAlgorithms, durableSnapshot.Policy.UserKeyAlgorithms = durableKeyAlgorithm, durableKeyAlgorithm
	state.Current = &durableSnapshot
	committed = true
	if oldListener != 0 && oldListener != next.Listener {
		closeEndpoint(oldListener)
	}
	syslog.Info("remote host key loaded", syslog.Str("fingerprint", state.Current.Fingerprint))
	if next.Listener == 0 {
		syslog.Info("remote listener disabled")
	} else {
		syslog.Info("remote listener active", syslog.Uint("port", uint64(policy.Port)), syslog.Uint("generation", next.Worker.Generation))
	}
	syslog.Info("remote configuration committed", syslog.Uint("generation", next.Worker.Generation))
	return nil
}
func serviceControl() {
	for i := 0; i < 8; i++ {
		var message ipc.Message
		status := remoteservice.Receive(control, 0, &message)
		if status == kcall.StatusNotFound {
			return
		}
		if status != kcall.StatusOK {
			syslog.Error("remote control receive failed", syslog.Uint("status", uint64(status)))
			sys.Exit(1)
		}
		notice(&message)
	}
}
func notice(message *ipc.Message) {
	if reconcileKeyReply(message) {
		return
	}
	if channel, ok := remoteapi.DecodeAdminNotice(message); ok {
		for i := range admins {
			if admins[i] == 0 {
				adminKeyRequests[i].Reset()
				admins[i] = channel
				return
			}
		}
		sys.HandleClose(channel)
		return
	}
	if p, a, ok := remoteapi.DecodeCounts(message); ok {
		pending, authenticated = p, a
		return
	}
	if renewed, ok := config.DecodeSessionNotice(message); ok {
		sys.HandleClose(configChannel)
		configChannel = renewed
		return
	}
	remoteservice.CloseHandles(message)
}
func workerCall(request *remoteapi.WorkerRequest, handles []handle.ID, out *ipc.Message) bool {
	sequence++
	var message ipc.Message
	if !remoteapi.EncodeWorkerRequest(request, handles, sequence, &message) || !remoteservice.Send(control, &message) {
		return false
	}
	correlation := sequence
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	deadline := remoteservice.Now() + budget
	for remoteservice.Now() < deadline {
		serviceTCP()
		serviceSigners()
		advance()
		status := remoteservice.Receive(control, 0, out)
		if status == kcall.StatusNotFound {
			var interests [kcall.WaitAnyMaxObjects]sys.Interest
			interests[0] = sys.Interest{Handle: control, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			interests[1] = sys.Interest{Handle: tcpChannel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			_, waitStatus := sys.WaitAny(&interests, 2, 1)
			if waitStatus != kcall.StatusOK && waitStatus != kcall.StatusTimeout {
				return false
			}
			continue
		}
		if status != kcall.StatusOK {
			return false
		}
		if out.Kind == ipc.KindNotification {
			notice(out)
			continue
		}
		if out.Correlation != correlation {
			if reconcileKeyReply(out) {
				continue
			}
			var closed remoteapi.WorkerReply
			if remoteapi.DecodeWorkerReply(out, out.Correlation, remoteapi.WorkerClose, &closed) {
				continue
			}
			remoteservice.CloseHandles(out)
			return false
		}
		var reply remoteapi.WorkerReply
		if !remoteapi.DecodeWorkerReply(out, correlation, request.Operation, &reply) {
			remoteservice.CloseHandles(out)
			return false
		}
		return reply.Result == remoteapi.ResultOK
	}
	return false
}
func serviceTCP() {
	for i := 0; i < 16; i++ {
		var message ipc.Message
		status := remoteservice.Receive(tcpChannel, 0, &message)
		if status == kcall.StatusNotFound {
			break
		}
		if status != kcall.StatusOK || !dispatch.Receive(&message) {
			remoteservice.CloseHandles(&message)
			syslog.Error("remote TCP transport violation")
			sys.Exit(1)
		}
	}
	dispatch.Flush()
}
func tcpCall(request tcpapi.Request) (tcpapi.Reply, bool) {
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	var reply tcpapi.Reply
	correlation, ok := dispatch.Start(request, nil)
	if !ok {
		return reply, false
	}
	deadline := remoteservice.Now() + budget
	for remoteservice.Now() < deadline {
		serviceTCP()
		serviceSigners()
		advance()
		if dispatch.Take(correlation, readBuffer[:], &reply) {
			dispatch.Flush()
			return reply, reply.Result == tcpapi.ResultOK
		}
		serviceControl()
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		interests[0] = sys.Interest{Handle: tcpChannel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		_, status := sys.WaitAny(&interests, 1, 1)
		if status != kcall.StatusOK && status != kcall.StatusTimeout {
			return reply, false
		}
	}
	// An uncompleted request cannot be safely reused on this shared session.
	syslog.Error("remote TCP service timeout", syslog.Uint("operation", uint64(request.Opcode)))
	sys.Exit(1)
	return reply, false
}
func closeEndpoint(endpoint uint32) {
	tcpCall(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint})
}
func serviceAdmins() {
	for i, channel := range admins {
		if channel == 0 {
			continue
		}
		var message ipc.Message
		status := remoteservice.Receive(channel, 0, &message)
		if status == kcall.StatusNotFound {
			continue
		}
		if status != kcall.StatusOK {
			sys.HandleClose(channel)
			admins[i] = 0
			continue
		}
		op, ok := remoteapi.DecodeAdminRequest(&message)
		var argument []byte
		requestCorrelation := message.Correlation
		if !ok {
			var complete bool
			op, argument, requestCorrelation, complete, ok = adminKeyRequests[i].Accept(&message)
			if ok && !complete {
				continue
			}
		} else if adminKeyRequests[i].Pending() {
			ok = false
		}
		if !ok {
			adminKeyRequests[i].Reset()
			remoteservice.CloseHandles(&message)
			sys.HandleClose(channel)
			admins[i] = 0
			continue
		}
		result := remoteapi.AdminOK
		text := state.Status(pending, authenticated)
		if op == remoteapi.AdminKeys || op == remoteapi.AdminKeyAdd || op == remoteapi.AdminKeyRemove {
			result, text = keyAdmin(op, argument)
		} else if op == remoteapi.AdminReload {
			if err := reload(); err != nil {
				state.Reject(err.Error())
				result = remoteapi.AdminUnavailable
				syslog.Warn("remote reload rejected", syslog.Str("reason", err.Error()))
				text = []byte(err.Error() + "\n")
			} else {
				text = state.Status(pending, authenticated)
			}
		}
		adminKeyRequests[i].Reset()
		if !sendAdminText(channel, op, result, text, requestCorrelation) {
			sys.HandleClose(channel)
			admins[i] = 0
		}
	}
}
func sendAdminText(channel handle.ID, op remoteapi.AdminOperation, result remoteapi.AdminResult, text []byte, correlation uint64) bool {
	if len(text) > remoteapi.MaxStatus*remoteapi.MaxStatusParts {
		return false
	}
	deadline := remoteservice.Now() + budget
	for part := 0; part < remoteapi.MaxStatusParts; part++ {
		n := min(len(text), remoteapi.MaxStatus)
		var reply ipc.Message
		more := len(text) > n
		if !remoteapi.EncodeAdminPart(op, result, text[:n], more, correlation, &reply) || !sendAdminWithin(channel, &reply, deadline) {
			return false
		}
		text = text[n:]
		if !more {
			return true
		}
	}
	return false
}
func advance() {
	var reply tcpapi.Reply
	if !pumpingRPC && accept != 0 && dispatch.Take(accept, nil, &reply) {
		accept = 0
		if reply.Result == tcpapi.ResultOK {
			if state.Current == nil || state.Current.Listener != acceptListener || !state.Current.Policy.Enabled {
				closeEndpoint(reply.Endpoint)
			} else {
				launchWorker(reply.Endpoint)
			}
		}
	}
	if !pumpingRPC && accept == 0 && state.Current != nil && state.Current.Listener != 0 && pending < remoteapi.MaxHandshakes && freeWorker() >= 0 {
		acceptListener = state.Current.Listener
		accept, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: acceptListener}, nil)
	}
	for round := 0; round < len(workers); round++ {
		i := (nextWorker + round) % len(workers)
		w := &workers[i]
		if w.endpoint == 0 {
			continue
		}
		if w.read != 0 && dispatch.Take(w.read, readBuffer[:], &reply) {
			w.read = 0
			if reply.Result == tcpapi.ResultOK {
				if !w.inputGone && !w.outputEOF && w.close == 0 && !w.toWorker.Put(readBuffer[:reply.Length]) {
					closeWorker(w)
				}
			} else if reply.Result == tcpapi.ResultEOF {
				w.tcpEOF = true
				if w.drainDeadline == 0 {
					w.drainDeadline = remoteservice.Now() + budget
				}
			} else {
				closeWorker(w)
			}
		}
		if w.write != 0 && dispatch.Take(w.write, nil, &reply) {
			w.write = 0
			if reply.Result != tcpapi.ResultOK || reply.Length == 0 || int(reply.Length) > w.written {
				closeWorker(w)
			} else {
				w.toTCP.Drop(int(reply.Length))
			}
		}
		if w.shutdown != 0 && dispatch.Take(w.shutdown, nil, &reply) {
			w.shutdown = 0
			if reply.Result != tcpapi.ResultOK {
				closeWorker(w)
			} else {
				w.shutdownDone = true
			}
		}
		if w.close != 0 {
			if !w.closeDone && dispatch.Take(w.close, nil, &reply) {
				w.closeDone = true
			}
			if w.closeDone && w.read == 0 && w.write == 0 && w.shutdown == 0 {
				*w = worker{}
			}
			continue
		}
		if !w.inputGone && !w.outputEOF && w.toWorker.Len() != 0 {
			data := w.toWorker.Peek(256)
			n, status := sys.StreamWrite(w.input, data)
			if n > 0 {
				w.toWorker.Drop(n)
			}
			if status == kcall.StatusPeerDied {
				w.inputGone = true
				sys.HandleClose(w.input)
				w.input = 0
				w.toWorker.Drop(w.toWorker.Len())
				if w.drainDeadline == 0 {
					w.drainDeadline = remoteservice.Now() + budget
				}
			} else if status != kcall.StatusOK && status != kcall.StatusResourceExhausted {
				closeWorker(w)
				continue
			}
		}
		if w.tcpEOF && !w.inputGone && w.toWorker.Len() == 0 {
			sys.HandleClose(w.input)
			w.input = 0
			w.inputGone = true
		}
		if w.drainDeadline != 0 && remoteservice.Now() >= w.drainDeadline {
			closeWorker(w)
			continue
		}
		if !w.outputEOF && w.toTCP.Free() >= 256 {
			n, status := sys.StreamRead(w.output, readBuffer[:256])
			if n > 0 {
				w.toTCP.Put(readBuffer[:n])
			}
			if status == kcall.StatusPeerDied {
				w.outputEOF = true
				w.drainDeadline = remoteservice.Now() + budget
				sys.HandleClose(w.output)
				w.output = 0
			}
			if status != kcall.StatusOK && status != kcall.StatusResourceExhausted && status != kcall.StatusPeerDied {
				closeWorker(w)
				continue
			}
		}
		if w.outputEOF && w.toTCP.Len() == 0 && w.write == 0 && w.shutdown == 0 && !w.shutdownDone {
			w.shutdown, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: w.endpoint}, nil)
		}
		if w.outputEOF && (w.shutdownDone && w.tcpEOF || remoteservice.Now() >= w.drainDeadline) {
			closeWorker(w)
			continue
		}
		if !w.tcpEOF && w.read == 0 && (w.outputEOF || w.toWorker.Free() >= tcpapi.SlotSize) {
			w.read, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: w.endpoint, Length: tcpapi.SlotSize}, nil)
		}
		if w.write == 0 && w.toTCP.Len() != 0 {
			data := w.toTCP.Peek(tcpapi.SlotSize)
			w.write, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: w.endpoint}, data)
			if w.write != 0 {
				w.written = len(data)
			}
		}
	}
	nextWorker = (nextWorker + 1) % len(workers)
	dispatch.Flush()
}
func freeWorker() int {
	for i := range workers {
		if workers[i].endpoint == 0 {
			return i
		}
	}
	return -1
}
func launchWorker(endpoint uint32) {
	index := freeWorker()
	if index < 0 {
		closeEndpoint(endpoint)
		return
	}
	memory, status := sys.MemoryCreate(1)
	if status != kcall.StatusOK {
		closeEndpoint(endpoint)
		return
	}
	base, size, status := sys.MemoryMapAnywhere(memory, true)
	if status != kcall.StatusOK || size != remoteapi.PolicyPageSize || !remoteapi.EncodePolicy(&state.Current.Worker, sys.Window(base, size)) {
		sys.HandleClose(memory)
		closeEndpoint(endpoint)
		return
	}
	sys.VMUnmap(base, size)
	immutable, status := sys.HandleDuplicate(memory, capability.Read|capability.Transfer)
	sys.HandleClose(memory)
	if status != kcall.StatusOK {
		closeEndpoint(endpoint)
		return
	}
	request := remoteapi.WorkerRequest{Operation: remoteapi.WorkerLaunch}
	var answer ipc.Message
	if !workerCall(&request, []handle.ID{immutable}, &answer) {
		syslog.Warn("remote worker admission refused")
		sys.HandleClose(immutable)
		closeEndpoint(endpoint)
		return
	}
	var reply remoteapi.WorkerReply
	remoteapi.DecodeWorkerReply(&answer, answer.Correlation, request.Operation, &reply)
	identity, ok := identities.Current()
	if !ok || !identities.Retain(identity) {
		syslog.Error("remote identity reference failed")
		remoteservice.CloseHandles(&answer)
		closeEndpoint(endpoint)
		sys.Exit(1)
	}
	workers[index] = worker{endpoint: endpoint, token: reply.Worker, input: answer.Handles[0], output: answer.Handles[1], signer: answer.Handles[2], identity: identity, identityHeld: true, generation: state.Current.Worker.Generation}
	syslog.Info("remote connection admitted", syslog.Uint("worker", reply.Worker))
}
func closeWorker(w *worker) {
	if w.close != 0 || w.endpoint == 0 {
		return
	}
	revokeSigner(w)
	syslog.Info("remote connection closing", syslog.Uint("worker", w.token))
	sys.HandleClose(w.input)
	sys.HandleClose(w.output)
	w.input, w.output = 0, 0
	if w.token != 0 {
		request := remoteapi.WorkerRequest{Operation: remoteapi.WorkerClose, Worker: w.token}
		var message ipc.Message
		sequence++
		if !remoteapi.EncodeWorkerRequest(&request, nil, sequence, &message) || !remoteservice.Send(control, &message) {
			syslog.Error("remote worker close request failed")
			sys.Exit(1)
		}
		w.token = 0
	}
	w.close, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: w.endpoint}, nil)
	if w.close == 0 {
		syslog.Error("remote endpoint close failed")
		sys.Exit(1)
	}
}
func revokeSigner(w *worker) {
	if w.signer != 0 {
		sys.HandleClose(w.signer)
		w.signer = 0
	}
	if w.identityHeld {
		identities.Release(w.identity)
		w.identityHeld = false
	}
}

func serviceSigners() {
	for i := range workers {
		w := &workers[i]
		if w.signer == 0 || w.close != 0 {
			continue
		}
		var message ipc.Message
		status := remoteservice.Receive(w.signer, 0, &message)
		if status == kcall.StatusPeerDied {
			revokeSigner(w)
			closeWorker(w)
			continue
		}
		if status == kcall.StatusNotFound {
			continue
		}
		var request remoteapi.SignRequest
		if status != kcall.StatusOK || !remoteapi.DecodeSignRequest(&message, &request) || request.Generation != w.generation || w.signSequence == ^uint64(0) || message.Correlation != w.signSequence+1 {
			remoteservice.CloseHandles(&message)
			closeWorker(w)
			continue
		}
		signature, ok := identities.Sign(w.identity, request.Data[:])
		var reply ipc.Message
		answer := remoteapi.SignReply{Generation: w.generation, Result: remoteapi.ResultOK, Signature: signature}
		if !ok || !remoteapi.EncodeSignReply(&answer, message.Correlation, &reply) || !remoteservice.Send(w.signer, &reply) {
			closeWorker(w)
		} else {
			w.signSequence++
		}
		clear(request.Data[:])
		clear(signature[:])
		clear(message.Payload[:])
		clear(reply.Payload[:])
	}
}

func main() {}
