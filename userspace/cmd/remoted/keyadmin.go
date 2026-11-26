//go:build goos_userspace

package main

import (
	"errors"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/remoteapi"
	"goos/userspace/remotepolicy"
	"goos/userspace/remoteservice"
	"goos/userspace/runtime/sys"
	"goos/userspace/utility"
	"strconv"
)

var remoteUpdateBusy bool
var adminKeyRequests [2]remoteapi.KeyRequestBuffer

// Each operation binds the transient key-only authority, then restores the
// controller's existing host-RW/key-R union. No new host grant is requested.
type keyFiles struct{ channel handle.ID }

func (f keyFiles) bind() func() {
	utility.BindVFS(f.channel)
	return func() { utility.BindVFS(vfsChannel) }
}
func (f keyFiles) Read(path string, limit int) ([]byte, error) {
	restore := f.bind()
	defer restore()
	return files.Read(path, limit)
}
func (f keyFiles) Create(path string, data []byte) error {
	restore := f.bind()
	defer restore()
	return files.Create(path, data)
}
func (f keyFiles) Rename(source, target string) error {
	restore := f.bind()
	defer restore()
	return files.Rename(source, target)
}
func (f keyFiles) Replace(source, target string) error {
	restore := f.bind()
	defer restore()
	return files.Replace(source, target)
}
func (f keyFiles) Remove(path string) error {
	restore := f.bind()
	defer restore()
	return files.Remove(path)
}

func sendAdminWithin(channel handle.ID, message *ipc.Message, deadline uint64) bool {
	var wire [ipc.WireMaxSize]byte
	n, ok := ipc.EncodeFrom(message, wire[:])
	if !ok || message.HandleCount != 0 {
		return false
	}
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	for remoteservice.Now() < deadline {
		status := sys.IPCSend(channel, wire[:n])
		if status == kcall.StatusOK {
			return true
		}
		if status != kcall.StatusResourceExhausted {
			return false
		}
		serviceTCP()
		serviceSigners()
		advance()
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		interests[0] = sys.Interest{Handle: tcpChannel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		if _, status = sys.WaitAny(&interests, 1, 1); status != kcall.StatusOK && status != kcall.StatusTimeout {
			return false
		}
	}
	return false
}

func queueKeyEnd(token uint64) {
	sequence++
	var request ipc.Message
	if !remoteapi.EncodeKeyControlRequest(remoteapi.KeyEnd, token, sequence, &request) || !remoteapi.SendWithin(control, &request, remoteservice.Now()+budget) {
		// Restart revokes every listener-owned writer and resets PID 1's lease.
		sys.Exit(1)
	}
}
func abandonKeyCall(op remoteapi.KeyControlOperation, token, correlation uint64) {
	if op == remoteapi.KeyBegin {
		queueKeyEnd(correlation)
	}
	if op == remoteapi.KeyEnd {
		queueKeyEnd(token)
	}
}
func reconcileKeyReply(message *ipc.Message) bool {
	if reply, ok := remoteapi.DecodeKeyControlReply(message, remoteapi.KeyBegin, message.Correlation); ok {
		remoteservice.CloseHandles(message)
		if reply.Result == remoteapi.ResultOK {
			queueKeyEnd(reply.Token)
		}
		return true
	}
	if _, ok := remoteapi.DecodeKeyControlReply(message, remoteapi.KeyEnd, message.Correlation); ok {
		return true
	}
	return false
}

func keyControlCall(op remoteapi.KeyControlOperation, token uint64) (remoteapi.KeyControlReply, handle.ID, error) {
	sequence++
	corr := sequence
	var request ipc.Message
	if !remoteapi.EncodeKeyControlRequest(op, token, corr, &request) || !remoteapi.SendWithin(control, &request, remoteservice.Now()+budget) {
		if op == remoteapi.KeyEnd {
			queueKeyEnd(token)
		}
		return remoteapi.KeyControlReply{}, 0, errors.New("remote key authority request failed")
	}
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	deadline := remoteservice.Now() + budget
	for remoteservice.Now() < deadline {
		serviceTCP()
		serviceSigners()
		advance()
		var message ipc.Message
		status := remoteservice.Receive(control, 0, &message)
		if status == kcall.StatusNotFound {
			var interests [kcall.WaitAnyMaxObjects]sys.Interest
			interests[0] = sys.Interest{Handle: control, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			interests[1] = sys.Interest{Handle: tcpChannel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			_, waitStatus := sys.WaitAny(&interests, 2, 1)
			if waitStatus != kcall.StatusOK && waitStatus != kcall.StatusTimeout {
				break
			}
			continue
		}
		if status != kcall.StatusOK {
			break
		}
		if message.Kind == ipc.KindNotification {
			notice(&message)
			continue
		}
		if message.Correlation != corr {
			if reconcileKeyReply(&message) {
				continue
			}
			var closed remoteapi.WorkerReply
			if remoteapi.DecodeWorkerReply(&message, message.Correlation, remoteapi.WorkerClose, &closed) {
				continue
			}
			remoteservice.CloseHandles(&message)
			break
		}
		reply, valid := remoteapi.DecodeKeyControlReply(&message, op, corr)
		if !valid || reply.Result != remoteapi.ResultOK {
			remoteservice.CloseHandles(&message)
			if !valid {
				abandonKeyCall(op, token, corr)
			}
			return remoteapi.KeyControlReply{}, 0, errors.New("remote key authority denied or ownership changed")
		}
		reply.Path = []byte(string(reply.Path))
		if message.HandleCount == 1 {
			return reply, message.Handles[0], nil
		}
		return reply, 0, nil
	}
	abandonKeyCall(op, token, corr)
	return remoteapi.KeyControlReply{}, 0, errors.New("remote key authority did not answer")
}

// rawKeyForFingerprint finds the raw 32-byte Ed25519 key whose SSH-blob
// fingerprint matches the canonical "SHA256:base64" value.
func rawKeyForFingerprint(data, fingerprint []byte) ([]byte, bool) {
	entries, err := remotepolicy.ListAuthorizedKeys(data)
	if err != nil {
		return nil, false
	}
	keys, err := remotepolicy.ParseAuthorizedKeys(data)
	if err != nil {
		return nil, false
	}
	for index, entry := range entries {
		if entry.Fingerprint != string(fingerprint) {
			continue
		}
		key, ok := keys.At(index)
		return key, ok
	}
	return nil, false
}

// syncKeyToAccount mirrors a committed key add or remove into the migrated
// account database, so the account database stays authoritative. It is
// best-effort: the legacy file keeps the change either way, and a mirror
// failure is reported rather than undoing the file mutation.
func syncKeyToAccount(remove bool, value [32]byte) bool {
	sequence++
	corr := sequence
	var request ipc.Message
	if !remoteapi.EncodeKeySyncRequest(remove, value, corr, &request) || !remoteapi.SendWithin(control, &request, remoteservice.Now()+budget) {
		return false
	}
	previous := pumpingRPC
	pumpingRPC = true
	defer func() { pumpingRPC = previous }()
	deadline := remoteservice.Now() + budget
	for remoteservice.Now() < deadline {
		serviceTCP()
		serviceSigners()
		advance()
		var message ipc.Message
		status := remoteservice.Receive(control, 0, &message)
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
		if message.Kind == ipc.KindNotification {
			notice(&message)
			continue
		}
		if message.Correlation != corr {
			remoteservice.CloseHandles(&message)
			return false
		}
		reply, valid := remoteapi.DecodeKeyControlReply(&message, remoteapi.KeySync, corr)
		remoteservice.CloseHandles(&message)
		return valid && reply.Result == remoteapi.ResultOK
	}
	return false
}

func keyReloadHost(policy remotepolicy.Policy) (*remotepolicy.HostKey, error) {
	if _, ok := identities.Current(); !ok {
		return nil, errors.New("host identity unavailable; use remote reload first")
	}
	return state.RetainedHost(policy.HostKey)
}
func keyPreflight(rev uint64, path string) error {
	deadline := remoteservice.Now() + budget
	before, err := revision(deadline)
	if err != nil || before != rev {
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
	if policy.AuthorizedKeys != path {
		return errors.New("authorized key path changed")
	}
	host, err := keyReloadHost(policy)
	if err != nil {
		return err
	}
	clear(host.Private)
	after, err := revision(remoteservice.Now() + budget)
	if err != nil || after != rev {
		return errors.New("remote configuration ownership changed")
	}
	return nil
}

func keyAdmin(op remoteapi.AdminOperation, argument []byte) (result remoteapi.AdminResult, text []byte) {
	result = remoteapi.AdminUnavailable
	if remoteUpdateBusy {
		return result, []byte("remote update busy\n")
	}
	remoteUpdateBusy = true
	defer func() { remoteUpdateBusy = false }()
	grant, channel, err := keyControlCall(remoteapi.KeyBegin, 0)
	if err != nil {
		return result, []byte(err.Error() + "\n")
	}
	defer func() {
		sys.HandleClose(channel)
		if _, _, endErr := keyControlCall(remoteapi.KeyEnd, grant.Token); endErr != nil {
			result = remoteapi.AdminUnavailable
			text = []byte(endErr.Error() + "\n")
		}
	}()
	check := func() error { _, _, err := keyControlCall(remoteapi.KeyCheck, grant.Token); return err }
	adapter := keyFiles{channel: channel}
	path := string(grant.Path)
	if op == remoteapi.AdminKeys {
		data, err := adapter.Read(path, remotepolicy.MaxAuthorizedKeysBytes)
		if errors.Is(err, remotepolicy.ErrNotFound) {
			data = nil
			err = nil
		}
		if err != nil {
			return result, []byte(err.Error() + "\n")
		}
		entries, err := remotepolicy.ListAuthorizedKeys(data)
		if err == nil {
			err = check()
		}
		if err != nil {
			return result, []byte(err.Error() + "\n")
		}
		output := make([]byte, 0, len(entries)*(51+remotepolicy.MaxKeyComment+1))
		for _, entry := range entries {
			output = append(output, entry.Fingerprint...)
			if entry.Comment != "" {
				output = append(output, ' ')
				output = append(output, entry.Comment...)
			}
			output = append(output, '\n')
		}
		return remoteapi.AdminOK, output
	}
	if err := keyPreflight(grant.Revision, path); err != nil {
		return result, []byte(err.Error() + "\n")
	}
	affected := string(argument)
	var syncRemove bool
	var syncValue [32]byte
	if op == remoteapi.AdminKeyAdd {
		added, err := remotepolicy.ParseAuthorizedKeys(argument)
		if err != nil || added.Len() != 1 {
			return remoteapi.AdminInvalid, []byte("expected one Ed25519 public key line\n")
		}
		public, _ := added.At(0)
		affected, _ = remotepolicy.Fingerprint(public)
		copy(syncValue[:], public)
	} else if op == remoteapi.AdminKeyRemove {
		if !remotepolicy.ValidFingerprint(argument) {
			return remoteapi.AdminInvalid, []byte("expected full canonical SHA256 fingerprint\n")
		}
		data, err := adapter.Read(path, remotepolicy.MaxAuthorizedKeysBytes)
		if errors.Is(err, remotepolicy.ErrNotFound) {
			data = nil
		}
		if err != nil {
			return result, []byte(err.Error() + "\n")
		}
		raw, ok := rawKeyForFingerprint(data, argument)
		if !ok {
			return remoteapi.AdminInvalid, []byte("authorized key fingerprint not found\n")
		}
		copy(syncValue[:], raw)
		syncRemove = true
	}
	update := remotepolicy.KeyUpdate{Files: adapter, Check: check, Reload: func(candidate []byte) (uint64, error) {
		if err := reloadCandidate(candidate, grant.Revision, path, adapter); err != nil {
			return 0, err
		}
		return state.Current.Worker.Generation, nil
	}}
	generation, err := update.Apply(path, func(old []byte) ([]byte, error) {
		if op == remoteapi.AdminKeyAdd {
			return remotepolicy.AddAuthorizedKey(old, argument)
		}
		if op == remoteapi.AdminKeyRemove {
			return remotepolicy.RemoveAuthorizedKey(old, argument)
		}
		return nil, errors.New("invalid authorized key operation")
	})
	if err != nil {
		return result, []byte(err.Error() + "\n")
	}
	if op == remoteapi.AdminKeyAdd || op == remoteapi.AdminKeyRemove {
		// The account database is the sole key authority: a key mutation is only
		// reported as committed once identityd has durably recorded it. A failed
		// mirror fails the command instead of leaving a stale file authoritative.
		if !syncKeyToAccount(syncRemove, syncValue) {
			syslog.Warn("remote key mirror into account database failed", syslog.Str("fingerprint", affected))
			return remoteapi.AdminUnavailable, []byte("remote key mirror into the account database failed\n")
		}
	}
	return remoteapi.AdminOK, []byte("authorized keys committed fingerprint=" + affected + " generation=" + strconv.FormatUint(generation, 10) + "\n")
}
