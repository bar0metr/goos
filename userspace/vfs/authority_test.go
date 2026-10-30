package vfsservice

import (
	"fmt"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"testing"
)

const remoteOwner vfs.Owner = 42

func scopeOK(t *testing.T, service *Service, slot uint8, path string, subtree bool, denied vfs.Rights) {
	t.Helper()
	if status := service.InstallScope(slot, []byte(path), subtree, denied); status != vfs.StatusOK {
		t.Fatalf("scope %s: %d", path, status)
	}
}

func remoteOK(t *testing.T, service *Service, request vfs.Request) vfs.Reply {
	t.Helper()
	reply := service.Dispatch(remoteOwner, request)
	if reply.Status != vfs.StatusOK {
		t.Fatalf("remote operation %d on %s -> %s: %d", request.Operation, request.Path, request.Target, reply.Status)
	}
	return reply
}

func deny(t *testing.T, service *Service, owner vfs.Owner, requests ...vfs.Request) {
	t.Helper()
	for _, request := range requests {
		reply := service.Dispatch(owner, request)
		if reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("owner %d operation %d on %s -> %s: %d, want denied", owner, request.Operation, request.Path, request.Target, reply.Status)
		}
	}
}

func protectedRemote(t *testing.T) (*Service, *testBlockDevice) {
	t.Helper()
	device := installedDevice(t)
	service := startedService(t, device)
	for _, path := range []string{"/state/security", "/state/security/ssh", "/home/user/keys"} {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(path)})
	}
	writeFile(t, service, "/state/security/ssh/host", "PRIVATE")
	writeFile(t, service, "/home/user/keys/authorized_keys", "PUBLIC")
	writeFile(t, service, "/state/public", "ordinary")
	scopeOK(t, service, 0, "/state/security/ssh", true, vfs.Read|vfs.Write)
	scopeOK(t, service, 1, "/home/user/keys/authorized_keys", false, 0)
	authority := vfs.Authority{Confined: true}
	authority.Grants[0], authority.Grants[1] = vfs.Read|vfs.Write, vfs.Read
	if status := service.SetAuthority(remoteOwner, authority); status != vfs.StatusOK {
		t.Fatal(status)
	}
	return service, device
}

func TestScopedAuthorityDeniesPrivateReadsAndAllMutations(t *testing.T) {
	service, _ := protectedRemote(t)
	for _, root := range []string{"/state/security/ssh", "/persist/state/SECURITY/SSH", "/state/security/../security/ssh"} {
		deny(t, service, testSession,
			vfs.Request{Operation: vfs.OperationOpen, Path: []byte(root + "/host"), Rights: vfs.Read},
			vfs.Request{Operation: vfs.OperationOpen, Path: []byte(root + "/host"), Rights: vfs.Read | vfs.Write},
			vfs.Request{Operation: vfs.OperationOpen, Path: []byte(root + "/host"), Rights: vfs.Write},
			vfs.Request{Operation: vfs.OperationReadDir, Path: []byte(root)},
			vfs.Request{Operation: vfs.OperationCreate, Path: []byte(root + "/new")},
			vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(root + "/new")},
			vfs.Request{Operation: vfs.OperationTruncate, Path: []byte(root + "/host")},
			vfs.Request{Operation: vfs.OperationUnlink, Path: []byte(root + "/host")},
			vfs.Request{Operation: vfs.OperationRename, Path: []byte(root + "/host"), Target: []byte("/state/leaked")},
			vfs.Request{Operation: vfs.OperationRename, Path: []byte("/state/public"), Target: []byte(root + "/incoming")},
			vfs.Request{Operation: vfs.OperationReplace, Path: []byte("/state/public"), Target: []byte(root + "/host")},
			vfs.Request{Operation: vfs.OperationReplace, Path: []byte(root + "/host"), Target: []byte("/state/public")},
		)
	}
	for _, path := range []string{"/state/security", "/persist/state/SECURITY"} {
		deny(t, service, testSession,
			vfs.Request{Operation: vfs.OperationRename, Path: []byte(path), Target: []byte("/state/moved")},
			vfs.Request{Operation: vfs.OperationUnlink, Path: []byte(path)},
		)
	}
	// Copy cannot acquire its source even when its destination is ordinary.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/copy")})
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/host"), Rights: vfs.Read})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/state/public")})
	refusesSystemChanges(t, service, "/persist/system")
}

func TestConfinedSessionCanStageReplaceAndReadOnlyAuthorizedKeys(t *testing.T) {
	service, _ := protectedRemote(t)
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/state/security/ssh/host.new")})
	file := remoteOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/persist/state/SECURITY/SSH/HOST.NEW"), Rights: vfs.Read | vfs.Write}).Handle
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationWrite, Handle: file, Data: []byte("NEW PRIVATE")})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: file})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationReplace, Path: []byte("/state/security/ssh/host.new"), Target: []byte("/state/security/ssh/host")})
	file = remoteOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/host"), Rights: vfs.Read}).Handle
	var data [64]byte
	if count, status := service.ReadFileAt(remoteOwner, file, 0, data[:]); status != vfs.StatusOK || string(data[:count]) != "NEW PRIVATE" {
		t.Fatalf("readat = %d %q", status, data[:count])
	}
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: file})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/state/security/ssh/host"), Offset: 3})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/state/security/ssh/host.pub")})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/state/security/ssh/host.pub")})
	auth := remoteOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/user/keys/authorized_keys"), Rights: vfs.Read}).Handle
	deny(t, service, remoteOwner,
		vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/public"), Rights: vfs.Read},
		vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/outside")},
		vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/user/keys/authorized_keys"), Rights: vfs.Write},
		vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/home/user/keys/authorized_keys")},
		vfs.MetadataRequest(nil, auth, ramfs.MetadataChange{SetReadOnly: true, ReadOnly: true}),
		vfs.Request{Operation: vfs.OperationRename, Path: []byte("/state/security/ssh/host"), Target: []byte("/tmp/escape")},
		vfs.Request{Operation: vfs.OperationRename, Path: []byte("/state/security/ssh"), Target: []byte("/state/security/ssh/new-root")},
	)
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: auth})
	service.CloseSession(remoteOwner)
	deny(t, service, remoteOwner, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/host"), Rights: vfs.Read})
}

func TestExactConfiguredStorageProtectsMissingNamesAndAliases(t *testing.T) {
	service := startedService(t, installedDevice(t))
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/user/custom")})
	scopeOK(t, service, 0, "/home/user/custom/HostKey", false, vfs.Read|vfs.Write)
	scopeOK(t, service, 1, "/home/user/custom/HostKey.new", false, vfs.Read|vfs.Write)
	authority := vfs.Authority{Confined: true}
	authority.Grants[0], authority.Grants[1] = vfs.Read|vfs.Write, vfs.Read|vfs.Write
	if service.SetAuthority(remoteOwner, authority) != vfs.StatusOK {
		t.Fatal("authority")
	}
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/home/USER/CUSTOM/HOSTKEY")})
	deny(t, service, remoteOwner,
		vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/user/custom/unrelated")},
		vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/user/custom/HostKey")},
	)
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/home/user/custom/hostkey.new")})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationReplace, Path: []byte("/home/user/custom/HOSTKEY.NEW"), Target: []byte("/home/user/custom/hostkey")})
	for _, path := range []string{"/home/user/custom/HostKey", "/persist/home/USER/CUSTOM/HOSTKEY"} {
		deny(t, service, testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte(path), Rights: vfs.Read}, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte(path)})
	}
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/persist/home/user/custom"), Target: []byte("/persist/home/user/moved")})
	// Exact scopes do not make unrelated siblings private or writable to remoted.
	writeFile(t, service, "/home/user/custom/public", "ordinary")
}

func TestScopedProtectionRevokesOldHandlesButNotUnrelatedOnes(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/state/private")})
	writeFile(t, service, "/state/private/key", "PRIVATE")
	writeFile(t, service, "/state/public", "PUBLIC")
	private := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/private/key"), Rights: vfs.Read | vfs.Write}).Handle
	public := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/public"), Rights: vfs.Read}).Handle
	scopeOK(t, service, 0, "/state/private", true, vfs.Read|vfs.Write)
	var data [64]byte
	for _, readAt := range []bool{false, true} {
		var status vfs.Status
		if readAt {
			_, status = service.ReadFileAt(testSession, private, 0, data[:])
		} else {
			_, status = service.ReadFile(testSession, private, data[:])
		}
		if status != vfs.StatusNotFound {
			t.Fatalf("revoked read = %d", status)
		}
	}
	if _, status := service.WriteFile(testSession, private, []byte("leak")); status != vfs.StatusNotFound {
		t.Fatalf("revoked write = %d", status)
	}
	if status := service.PrepareExecutable(testSession, private); status != vfs.StatusNotFound {
		t.Fatalf("revoked prepare = %d", status)
	}
	if count, status := service.ReadFileAt(testSession, public, 0, data[:]); status != vfs.StatusOK || string(data[:count]) != "PUBLIC" {
		t.Fatalf("public read = %d", status)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: public})
}

func TestScopedProtectionSurvivesFilesystemRestartAndWithdraw(t *testing.T) {
	service, device := protectedRemote(t)
	if service.Publish() != vfs.StatusOK {
		t.Fatal("publish")
	}
	service.DetachFilesystem()
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/host"), Rights: vfs.Read}); reply.Status == vfs.StatusOK {
		t.Fatal("detached backend exposed private data")
	}
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("reattach")
	}
	if service.Withdraw() != vfs.StatusOK {
		t.Fatal("withdraw")
	}
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/persist/state/SECURITY/SSH/HOST"), Rights: vfs.Read})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/host"), Rights: vfs.Read})
	service.CloseSession(remoteOwner)
	if status := service.UnmountFilesystem(); status != vfs.StatusBusy {
		t.Fatalf("scoped backend unmount = %d", status)
	}
}

func TestOverlappingHostSubtreeDoesNotWidenAuthorizedKeysGrant(t *testing.T) {
	service, _ := protectedRemote(t)
	// Configuration may put authorized_keys inside the writable host-key tree.
	authority := vfs.Authority{Confined: true}
	authority.Grants[0] = vfs.Read | vfs.Write
	if service.SetAuthority(remoteOwner, authority) != vfs.StatusOK {
		t.Fatal("authority")
	}
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/state/security/ssh/authorized_keys")})
	scopeOK(t, service, 2, "/state/security/ssh/authorized_keys", false, 0)
	authority.Grants[2] = vfs.Read
	if service.SetAuthority(remoteOwner, authority) != vfs.StatusOK {
		t.Fatal("authority")
	}
	file := remoteOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/authorized_keys"), Rights: vfs.Read}).Handle
	deny(t, service, remoteOwner,
		vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/security/ssh/authorized_keys"), Rights: vfs.Write},
		vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/state/security/ssh/authorized_keys")},
		vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/state/security/ssh/authorized_keys")},
		vfs.MetadataRequest(nil, file, ramfs.MetadataChange{SetReadOnly: true, ReadOnly: true}),
	)
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: file})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/state/security/ssh/host")})
}

func TestListingsHideScopedFilesAndDirectoriesThroughAliases(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/state/keys")})
	writeFile(t, service, "/state/keys/secret", "PRIVATE")
	writeFile(t, service, "/state/keys/public", "PUBLIC")
	scopeOK(t, service, 0, "/state/keys/secret", false, vfs.Read|vfs.Write)
	for _, path := range []string{"/state/keys", "/persist/state/KEYS"} {
		cursor := uint64(0)
		seen := false
		for {
			reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte(path), Offset: cursor})
			if reply.Status == vfs.StatusNotFound {
				break
			}
			if reply.Status != vfs.StatusOK || string(reply.Name) != "public" {
				t.Fatalf("listing %s: status=%d name=%q", path, reply.Status, reply.Name)
			}
			seen, cursor = true, uint64(reply.Cursor)
		}
		if !seen {
			t.Fatal("unrelated sibling disappeared")
		}
	}
	// The protected directory itself is also hidden from its ordinary parent.
	scopeOK(t, service, 1, "/state/keys", true, vfs.Read|vfs.Write)
	for cursor := uint64(0); ; {
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/state"), Offset: cursor})
		if reply.Status == vfs.StatusNotFound {
			break
		}
		if reply.Status != vfs.StatusOK || string(reply.Name) == "keys" {
			t.Fatalf("parent listing: status=%d name=%q", reply.Status, reply.Name)
		}
		cursor = uint64(reply.Cursor)
	}
}

func TestMissingExactScopeUsesFATUnicodeUpperMapping(t *testing.T) {
	service := startedService(t, testFAT32Device())
	scopeOK(t, service, 0, "/state/IKey", false, vfs.Read|vfs.Write)
	// FAT uppercases dotless i to I; Unicode EqualFold does not equate them.
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/state/ıkey")})
	authority := vfs.Authority{Confined: true}
	authority.Grants[0] = vfs.Read | vfs.Write
	if service.SetAuthority(remoteOwner, authority) != vfs.StatusOK {
		t.Fatal("authority")
	}
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/state/ıkey")})
	deny(t, service, testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/IKEY"), Rights: vfs.Read})
}

func listNames(t *testing.T, service *Service, owner vfs.Owner, path string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for cursor := uint64(0); ; {
		reply := service.Dispatch(owner, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte(path), Offset: cursor})
		if reply.Status == vfs.StatusNotFound {
			return names
		}
		if reply.Status != vfs.StatusOK {
			t.Fatalf("list %s: %d", path, reply.Status)
		}
		names[string(reply.Name)] = true
		cursor = uint64(reply.Cursor)
	}
}

// Listing many unrelated FAT32 entries costs one walk of a protected subtree
// elsewhere on the volume, through the same requests a client sends.
func TestFATListingAuthorityWorkIsBounded(t *testing.T) {
	service, _ := protectedRemote(t)
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/tools")})
	const entries = 120
	for index := 0; index < entries; index++ {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(fmt.Sprintf("/home/tools/tool%d", index))})
	}
	before, readsBefore := service.ListingCounters()
	if names := listNames(t, service, testSession, "/home/tools"); len(names) != entries {
		t.Fatalf("listed %d", len(names))
	}
	after, reads := service.ListingCounters()
	if walks := after.ScopeContains - before.ScopeContains; walks != 1 {
		t.Fatalf("subtree walks = %d", walks)
	}
	if hidden := after.ScopeBackendReadDir - before.ScopeBackendReadDir; hidden > 8 {
		t.Fatalf("authority directory reads = %d", hidden)
	}
	if opens := after.ScopeOpenName - before.ScopeOpenName; opens > 2 {
		t.Fatalf("exact lookups = %d", opens)
	}
	if total := reads - readsBefore; total > entries+10 {
		t.Fatalf("directory exchanges = %d for %d entries", total, entries)
	}
}

func TestFATListingKeepsCaseInsensitiveIdentity(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/state/keys")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/state/keys/nested")})
	writeFile(t, service, "/state/keys/secret", "PRIVATE")
	writeFile(t, service, "/state/keys/public", "PUBLIC")
	scopeOK(t, service, 0, "/persist/STATE/KEYS/SECRET", false, vfs.Read|vfs.Write)
	scopeOK(t, service, 1, "/persist/STATE/KEYS/NESTED", true, vfs.Read|vfs.Write)
	for _, path := range []string{"/state/keys", "/persist/state/KEYS", "/persist/STATE/keys"} {
		names := listNames(t, service, testSession, path)
		if len(names) != 1 || !names["public"] {
			t.Fatalf("list %s = %v", path, names)
		}
	}
	// A rename into the exact name takes its protection on the next listing.
	authority := vfs.Authority{}
	authority.Grants[0] = vfs.Read | vfs.Write
	if status := service.SetAuthority(remoteOwner, authority); status != vfs.StatusOK {
		t.Fatal(status)
	}
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/state/keys/secret")})
	remoteOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/state/keys/public"), Target: []byte("/state/keys/SECRET")})
	if names := listNames(t, service, testSession, "/state/keys"); len(names) != 0 {
		t.Fatalf("renamed into exact scope: %v", names)
	}
}

// A restarted filesystem service answers afresh: no scope decision survives it.
func TestFATListingForgetsScopeWorkAcrossRestart(t *testing.T) {
	service, device := protectedRemote(t)
	listNames(t, service, testSession, "/state")
	service.DetachFilesystem()
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/state")}); reply.Status == vfs.StatusOK {
		t.Fatal("detached backend listed")
	}
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("reattach")
	}
	before, _ := service.ListingCounters()
	if names := listNames(t, service, testSession, "/state/security"); names["ssh"] {
		t.Fatalf("restart exposed protected directory: %v", names)
	}
	after, _ := service.ListingCounters()
	if after.ScopeContains == before.ScopeContains {
		t.Fatal("scope decision survived the restart")
	}
}
