//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	configproto "goos/userspace/config"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
	"goos/userspace/remoteapi"
	"goos/userspace/remotepolicy"
)

// This guard is owned by the listener incarnation, never by a shell or worker.
// Its VFS grant contains no host-private scopes. Scope slots are never recycled.
var remoteKeyGuard remoteKeyLease

func resetRemoteKeyGuard() { remoteKeyGuard = remoteKeyLease{} }

func remoteKeyConfig() (string, string, uint64, bool) {
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		return "", "", 0, false
	}
	defer closeOpen(settings)
	before, ok := remoteConfigRevision(settings)
	if !ok {
		return "", "", 0, false
	}
	host, ok := remoteConfigPath(settings, "system.remote.host-key")
	if !ok {
		return "", "", 0, false
	}
	keys, ok := remoteConfigPath(settings, "system.remote.authorized-keys")
	if !ok {
		return "", "", 0, false
	}
	after, ok := remoteConfigRevision(settings)
	return host, keys, before, ok && after == before && remoteapi.ValidHostPath([]byte(host))
}

func remoteKeyScopes(host, keys string) bool {
	// Reserve all required staging slots before installing any. Existing host
	// scopes and old authorized paths retain their protection and consume slots.
	names := []string{keys + remotepolicy.KeyStageSuffix, keys + remotepolicy.KeyPriorSuffix}
	missing := 0
	for _, name := range names {
		found := false
		for i := 0; i < remoteScopeCount; i++ {
			s := &remoteScopes[i]
			if !s.subtree && equalBytes(s.path[:s.size], name) {
				if !s.staging {
					return false
				}
				found = true
			}
		}
		if !found {
			missing++
		}
	}
	if remoteScopeCount+missing > remoteScopeMax || !ensureRemotePolicyScopes(host, keys) || remoteScopeCount+missing > remoteScopeMax {
		return false
	}
	for _, name := range names {
		found := false
		for i := 0; i < remoteScopeCount; i++ {
			s := &remoteScopes[i]
			if !s.subtree && s.staging && equalBytes(s.path[:s.size], name) {
				found = true
			}
		}
		if !found {
			index := remoteScopeCount
			if !rememberRemoteScope(name, false, vfs.Read|vfs.Write) {
				return false
			}
			remoteScopes[index].staging = true
		}
	}
	for i := 0; i < remoteScopeCount; i++ {
		if !installRemoteScope(i) {
			return false
		}
	}
	return true
}

func openRemoteKeyAuthority(keys string) (handle.ID, string) {
	authority := remoteKeyAuthority(remoteScopes[:remoteScopeCount], keys)
	// VFS intersects overlapping scopes by backend/node identity, including
	// mounts and case aliases. No grant here can override a private host scope.
	return openRemoteAuthoritySession(authority)
}

func remoteKeyControl(message *ipc.Message) bool {
	// KeySync mirrors a key mutation the listener already committed to the
	// authorized_keys file into the migrated account, so the account database
	// stays authoritative. It fails closed when migration has not committed.
	if remove, value, corr, ok := remoteapi.DecodeKeySyncRequest(message); ok {
		result := remoteapi.ResultPermissionDenied
		if name, ok := legacyRemoteAccount(); ok {
			request := &identityproto.Request{Opcode: identityproto.AdminAddKey, Name: name, Key: value[:]}
			if remove {
				var key identity.Key
				copy(key[:], value[:])
				fingerprint := identity.Fingerprint(key)
				request = &identityproto.Request{Opcode: identityproto.AdminRemoveKey, Name: name, Fingerprint: fingerprint[:]}
			}
			if reply, ok := identityAdminCall(request); ok && reply.Result == identityproto.ResultOK {
				result = remoteapi.ResultOK
			}
		}
		var out ipc.Message
		if !remoteapi.EncodeKeyControlReply(remoteapi.KeyControlReply{Operation: remoteapi.KeySync, Result: result}, 0, corr, &out) || !sendMessage(uint64(remoteControl), out) {
			return false
		}
		return true
	}
	op, token, ok := remoteapi.DecodeKeyControlRequest(message)
	if !ok {
		return false
	}
	reply := remoteapi.KeyControlReply{Operation: op, Result: remoteapi.ResultPermissionDenied}
	var files handle.ID
	switch op {
	case remoteapi.KeyBegin:
		if remoteKeyGuard.token != 0 {
			reply.Result = remoteapi.ResultLimit
			break
		}
		host, keys, revision, valid := remoteKeyConfig()
		if !valid || !remoteKeyScopes(host, keys) {
			break
		}
		var reason string
		files, reason = openRemoteKeyAuthority(keys)
		if reason != "" {
			reply.Result = remoteapi.ResultUnavailable
			break
		}
		_, current, after, valid := remoteKeyConfig()
		if !valid || after != revision || current != keys {
			closeOpen(files)
			files = 0
			break
		}
		if !remoteKeyGuard.begin(message.Correlation, revision, keys) {
			closeOpen(files)
			files = 0
			break
		}
		reply.Result, reply.Token, reply.Revision, reply.Path = remoteapi.ResultOK, remoteKeyGuard.token, revision, remoteKeyGuard.path[:remoteKeyGuard.size]
	case remoteapi.KeyCheck:
		if token == remoteKeyGuard.token && token != 0 {
			_, keys, revision, valid := remoteKeyConfig()
			if valid && remoteKeyGuard.owns(token, revision, keys) {
				reply.Result = remoteapi.ResultOK
			}
		}
	case remoteapi.KeyEnd:
		if remoteKeyGuard.end(token) {
			reply.Result = remoteapi.ResultOK
		}
	}
	var out ipc.Message
	if !remoteapi.EncodeKeyControlReply(reply, files, message.Correlation, &out) || !sendMessage(uint64(remoteControl), out) {
		closeOpen(files)
		if op == remoteapi.KeyBegin {
			resetRemoteKeyGuard()
		}
	}
	return true
}
