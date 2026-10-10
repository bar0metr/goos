//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	configproto "goos/userspace/config"
	"goos/userspace/remoteapi"
	"goos/userspace/runtime/sys"
)

const (
	remotePrivateDirectory = "/state/security/ssh"
	remoteDefaultHost      = remotePrivateDirectory + "/ssh_host_ed25519_key"
	remoteDefaultKeys      = "/home/user/.ssh/authorized_keys"
)

type remoteScope struct {
	path      [96]byte
	size      uint8
	subtree   bool
	denied    vfs.Rights
	installed bool
}

var (
	remoteScopes          [vfs.MaxScopes]remoteScope
	remoteScopeCount      int
	remoteScopesInstalled bool
	remotePolicyScratch   remoteapi.WorkerPolicy
)

// copyRemotePolicy removes the controller's writable alias before launching a
// worker. The original snapshot is never mapped in a shell or a command.
func copyRemotePolicy(source handle.ID) (handle.ID, uint8, bool) {
	base, size, status := sys.MemoryMapAnywhere(source, false)
	if status != kcall.StatusOK {
		return 0, 0, false
	}
	defer sys.VMUnmap(base, size)
	if size != remoteapi.PolicyPageSize {
		return 0, 0, false
	}
	memory, status := sys.MemoryCreate(1)
	if status != kcall.StatusOK {
		return 0, 0, false
	}
	target, targetSize, status := sys.MemoryMapAnywhere(memory, true)
	if status != kcall.StatusOK || targetSize != remoteapi.PolicyPageSize {
		if status == kcall.StatusOK {
			sys.VMUnmap(target, targetSize)
		}
		closeOpen(memory)
		return 0, 0, false
	}
	page := sys.Window(target, targetSize)
	copy(page, sys.Window(base, size))
	valid := remoteapi.DecodePolicy(page, &remotePolicyScratch)
	limit := remotePolicyScratch.MaxSessions
	for index := range remotePolicyScratch.Keys {
		clear(remotePolicyScratch.Keys[index][:])
	}
	sys.VMUnmap(target, targetSize)
	if !valid {
		closeOpen(memory)
		return 0, 0, false
	}
	return memory, limit, true
}

func remotePaths() (string, string, bool) {
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		return "", "", false
	}
	defer closeOpen(settings)
	before, ok := remoteConfigRevision(settings)
	if !ok {
		return "", "", false
	}
	host, ok := remoteConfigPath(settings, "system.remote.host-key")
	if !ok {
		return "", "", false
	}
	keys, ok := remoteConfigPath(settings, "system.remote.authorized-keys")
	if !ok {
		return "", "", false
	}
	after, ok := remoteConfigRevision(settings)
	return host, keys, ok && before == after && remoteapi.ValidHostPath([]byte(host))
}

func remoteConfigRevision(settings handle.ID) (uint64, bool) {
	correlation++
	request, ok := configproto.EncodeRequest(configproto.Request{Operation: configproto.OperationRevision}, correlation)
	if !ok || !sendMessage(uint64(settings), request) {
		return 0, false
	}
	message, status := receive(uint64(settings), vfsSessionDeadline)
	reply, ok := configproto.DecodeReply(message)
	if status != ipc.StatusOK || !ok || message.Correlation != correlation || reply.Operation != configproto.OperationRevision || reply.Status != configproto.StatusOK || reply.Value.Kind != configproto.KindUint || reply.Value.Uint == 0 {
		return 0, false
	}
	return reply.Value.Uint, true
}

func remoteConfigPath(settings handle.ID, key string) (string, bool) {
	correlation++
	request, ok := configproto.EncodeRequest(configproto.Request{Operation: configproto.OperationGet, Key: []byte(key)}, correlation)
	if !ok || !sendMessage(uint64(settings), request) {
		return "", false
	}
	message, status := receive(uint64(settings), vfsSessionDeadline)
	reply, ok := configproto.DecodeReply(message)
	if status != ipc.StatusOK || !ok || message.Correlation != correlation || reply.Operation != configproto.OperationGet || reply.Status != configproto.StatusOK || reply.Value.Kind != configproto.KindText {
		return "", false
	}
	value := string(reply.Value.Bytes())
	return value, remoteapi.ValidAuthorityPath([]byte(value))
}

// Protection is installed even while SSH is disabled. User sessions cannot
// precede it, and a restarted VFS gets the same immutable scope slots back.
func protectRemoteStorage() bool {
	if remoteScopesInstalled {
		return true
	}
	files, reason := openRemoteAuthorityExisting()
	if reason != "" {
		return false
	}
	defer closeOpen(files)
	if remoteScopeCount == 0 {
		if !ensureRemoteParents(files, remotePrivateDirectory+"/placeholder") {
			return false
		}
		if !rememberRemoteScope(remotePrivateDirectory, true, vfs.Read|vfs.Write) {
			return false
		}
	}
	// Recreate directories before restoring any of the scope restrictions.
	for index := 0; index < remoteScopeCount; index++ {
		scope := &remoteScopes[index]
		path := bytesString(scope.path[:scope.size])
		if scope.subtree {
			path += "/placeholder"
		}
		if !ensureRemoteParents(files, path) {
			return false
		}
	}
	for index := 0; index < remoteScopeCount; index++ {
		if !installRemoteScope(index) {
			return false
		}
	}
	// Preserve valid configured host paths even when another setting is invalid.
	// Candidate-specific read/write authority remains confined to WorkerAuthority.
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		return false
	}
	host, valid := remoteConfigPath(settings, "system.remote.host-key")
	closeOpen(settings)
	if valid && remoteapi.ValidHostPath([]byte(host)) && !protectConfiguredRemoteHost(files, host) {
		return false
	}
	remoteScopesInstalled = true
	return true
}

func ensureRemoteParents(files handle.ID, path string) bool {
	for index := 1; index < len(path); index++ {
		if path[index] != '/' {
			continue
		}
		prefix := []byte(path[:index])
		stat, ok := callSession(files, vfs.Request{Operation: vfs.OperationStat, Path: prefix})
		if ok && stat.Status == vfs.StatusOK && stat.Kind == ramfs.KindDirectory {
			continue
		}
		reply, ok := callSession(files, vfs.Request{Operation: vfs.OperationMkdir, Path: prefix})
		if !ok || reply.Status != vfs.StatusOK {
			return false
		}
	}
	return true
}

func rememberRemoteScope(path string, subtree bool, denied vfs.Rights) bool {
	for index := 0; index < remoteScopeCount; index++ {
		scope := &remoteScopes[index]
		if scope.subtree == subtree && scope.denied == denied && equalBytes(scope.path[:scope.size], path) {
			return true
		}
	}
	if remoteScopeCount == len(remoteScopes) || len(path) > len(remoteScopes[0].path) {
		return false
	}
	scope := &remoteScopes[remoteScopeCount]
	scope.size = uint8(copy(scope.path[:], path))
	scope.subtree, scope.denied = subtree, denied
	remoteScopeCount++
	return true
}

func installRemoteScope(index int) bool {
	scope := &remoteScopes[index]
	if scope.installed {
		return true
	}
	correlation++
	request, ok := vfs.InstallScopeRequest(uint8(index), scope.path[:scope.size], scope.subtree, scope.denied, 0, correlation)
	if !ok || !sendMessage(vfsControlHandle, request) {
		return false
	}
	message, status := receive(vfsControlHandle, vfsSessionDeadline)
	result, ok := vfs.DecodeAddSessionReply(message)
	scope.installed = status == ipc.StatusOK && ok && result == vfs.StatusOK && message.Correlation == correlation
	return scope.installed
}

func ensureRemotePolicyScopes(host, keys string) bool {
	if !remoteapi.ValidHostPath([]byte(host)) || !remoteapi.ValidAuthorityPath([]byte(keys)) {
		return false
	}
	files, reason := openRemoteAuthorityExisting()
	if reason != "" {
		return false
	}
	defer closeOpen(files)
	if !ensureRemoteParents(files, host) || !ensureRemoteParents(files, keys) {
		return false
	}
	before := remoteScopeCount
	insideDefault := len(host) > len(remotePrivateDirectory) && host[:len(remotePrivateDirectory)] == remotePrivateDirectory && host[len(remotePrivateDirectory)] == '/'
	needed := 1
	if !insideDefault {
		needed += 4
	}
	// Account for already installed candidates without allocating duplicate slots.
	candidates := []string{keys}
	if !insideDefault {
		candidates = append(candidates, host, host+".new", host+".pub", host+".pub.new")
	}
	missing := 0
	for _, path := range candidates {
		found := false
		for index := 0; index < remoteScopeCount; index++ {
			scope := &remoteScopes[index]
			if !scope.subtree && equalBytes(scope.path[:scope.size], path) {
				found = true
				break
			}
		}
		if !found {
			missing++
		}
	}
	if missing > needed || remoteScopeCount+missing > len(remoteScopes) {
		return false
	}
	if !insideDefault {
		for _, path := range candidates[1:] {
			if !rememberRemoteScope(path, false, vfs.Read|vfs.Write) {
				return false
			}
		}
	}
	if !rememberRemoteScope(keys, false, 0) {
		return false
	}
	_ = before
	for index := 0; index < remoteScopeCount; index++ {
		if !installRemoteScope(index) {
			return false
		}
	}
	return true
}

func openRemoteAuthority() (handle.ID, string) {
	if !protectRemoteStorage() {
		return 0, "REMOTE_STORAGE_PROTECTION_FAILED"
	}
	return openRemoteAuthoritySession(vfs.Authority{Confined: true})
}

func openRemoteAuthorityExisting() (handle.ID, string) {
	authority := vfs.Authority{Confined: false}
	for index := 0; index < remoteScopeCount; index++ {
		if !remoteScopes[index].installed {
			continue
		}
		if remoteScopes[index].denied != 0 {
			authority.Grants[index] = vfs.Read | vfs.Write
		} else {
			authority.Grants[index] = vfs.Read
		}
	}
	// During scope setup PID 1 also creates parent directories outside the
	// confined service's authority. The session handed to remoted is confined.
	return openRemoteAuthoritySession(authority)
}

func openRemoteConfinedAuthority() (handle.ID, string) {
	authority := vfs.Authority{Confined: true}
	for index := 0; index < remoteScopeCount; index++ {
		if !remoteScopes[index].installed {
			continue
		}
		if remoteScopes[index].denied != 0 {
			authority.Grants[index] = vfs.Read | vfs.Write
		} else {
			authority.Grants[index] = vfs.Read
		}
	}
	return openRemoteAuthoritySession(authority)
}

func remoteAuthority(request *remoteapi.WorkerRequest, requestCorrelation uint64) {
	host, keys, ok := remotePaths()
	result := remoteapi.ResultPermissionDenied
	var files, settings handle.ID
	if ok && equalBytes(request.HostPath, host) && equalBytes(request.AuthorizedKeys, keys) {
		result = remoteapi.ResultUnavailable
		if ensureRemotePolicyScopes(host, keys) {
			var reason string
			files, reason = openRemoteConfinedAuthority()
			if reason == "" {
				settings, ok = openConfigSession("system.remote.", configproto.Read)
				if ok {
					result = remoteapi.ResultOK
				}
			}
		}
	}
	var reply ipc.Message
	var handles []handle.ID
	if result == remoteapi.ResultOK {
		handles = []handle.ID{files, settings}
	}
	encoded := remoteapi.EncodeWorkerAuthorityReply(&remoteapi.WorkerReply{Operation: remoteapi.WorkerAuthority, Result: result}, handles, requestCorrelation, &reply)
	if !encoded || !sendMessage(uint64(remoteControl), reply) || result != remoteapi.ResultOK {
		closeOpen(files)
		closeOpen(settings)
	}
}

func openRemoteAuthoritySession(authority vfs.Authority) (handle.ID, string) {
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0, "REMOTE_AUTHORITY_CHANNEL_FAILED"
	}
	correlation++
	request, ok := vfs.AddSessionWithAuthorityRequest(handle.ID(pair[1]), authority, correlation)
	if !ok || !sendMessage(vfsControlHandle, request) {
		closePair(pair)
		return 0, "REMOTE_AUTHORITY_SEND_FAILED"
	}
	message, status := receive(vfsControlHandle, vfsSessionDeadline)
	result, ok := vfs.DecodeAddSessionReply(message)
	if status != ipc.StatusOK || !ok || result != vfs.StatusOK || message.Correlation != correlation {
		closeOpen(handle.ID(pair[0]))
		return 0, "REMOTE_AUTHORITY_REFUSED"
	}
	return handle.ID(pair[0]), ""
}

func protectConfiguredRemoteHost(files handle.ID, host string) bool {
	if len(host) > len(remotePrivateDirectory) && host[:len(remotePrivateDirectory)] == remotePrivateDirectory && host[len(remotePrivateDirectory)] == '/' {
		return true
	}
	paths := []string{host, host + ".new", host + ".pub", host + ".pub.new"}
	missing := 0
	for _, path := range paths {
		found := false
		for i := 0; i < remoteScopeCount; i++ {
			if !remoteScopes[i].subtree && equalBytes(remoteScopes[i].path[:remoteScopes[i].size], path) {
				found = true
				break
			}
		}
		if !found {
			missing++
		}
	}
	if remoteScopeCount+missing > len(remoteScopes) || !ensureRemoteParents(files, host) {
		return false
	}
	for _, path := range paths {
		if !rememberRemoteScope(path, false, vfs.Read|vfs.Write) {
			return false
		}
	}
	for i := 0; i < remoteScopeCount; i++ {
		if !installRemoteScope(i) {
			return false
		}
	}
	return true
}
