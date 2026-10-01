package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"testing"
)

// installedDevice is a volume with an installed system, written through a
// service that saw no system directory when it mounted the volume and so
// protected nothing, as a host tool writes one.
func installedDevice(t *testing.T) *testBlockDevice {
	t.Helper()
	device := testFAT32Device()
	service := startedService(t, device)
	for _, directory := range []string{"/persist/system", "/persist/system/bin", "/home/user"} {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(directory)})
	}
	writeFile(t, service, "/persist/system/commands", "ls\tlist\n")
	writeFile(t, service, "/persist/system/bin/ls", "PROGRAM")
	writeFile(t, service, "/home/user/ls", "USER")
	if status := service.UnmountFilesystem(); status != vfs.StatusOK {
		t.Fatalf("unmount = %d", status)
	}
	return device
}

func dispatchOK(t *testing.T, service *Service, request vfs.Request) vfs.Reply {
	t.Helper()
	reply := service.Dispatch(testSession, request)
	if reply.Status != vfs.StatusOK {
		t.Fatalf("operation %d on %s = %d", request.Operation, request.Path, reply.Status)
	}
	return reply
}

func writeFile(t *testing.T, service *Service, path, payload string) {
	t.Helper()
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)})
	opened := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(path)})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte(payload)})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

// refusesSystemChanges tries every mutation of the system directory through
// the names a client could use and requires each to be refused.
func refusesSystemChanges(t *testing.T, service *Service, roots ...string) {
	t.Helper()
	for _, root := range roots {
		attempts := []vfs.Request{
			{Operation: vfs.OperationCreate, Path: []byte(root + "/bin/backdoor")},
			{Operation: vfs.OperationMkdir, Path: []byte(root + "/extra")},
			{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(root + "/commands")},
			{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: []byte(root + "/bin/ls")},
			{Operation: vfs.OperationTruncate, Path: []byte(root + "/commands")},
			{Operation: vfs.OperationUnlink, Path: []byte(root + "/bin/ls")},
			{Operation: vfs.OperationRename, Path: []byte(root + "/commands"), Target: []byte(root + "/commands.old")},
			{Operation: vfs.OperationRename, Path: []byte(root + "/bin/ls"), Target: []byte("/persist/home/ls")},
			{Operation: vfs.OperationRename, Path: []byte("/home/user/ls"), Target: []byte(root + "/bin/ls2")},
			{Operation: vfs.OperationReplace, Path: []byte("/home/user/ls"), Target: []byte(root + "/bin/ls")},
			{Operation: vfs.OperationReplace, Path: []byte(root + "/bin/ls"), Target: []byte("/home/user/ls")},
		}
		for _, request := range attempts {
			if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied {
				t.Fatalf("operation %d on %s -> %s = %d, want permission denied", request.Operation, request.Path, request.Target, reply.Status)
			}
		}
	}
	// The directory itself cannot be moved or removed under any of its names.
	for _, alias := range []string{"/persist/system", "/persist/SYSTEM", "/persist/System"} {
		for _, request := range []vfs.Request{
			{Operation: vfs.OperationRename, Path: []byte(alias), Target: []byte("/persist/old-system")},
			{Operation: vfs.OperationUnlink, Path: []byte(alias)},
		} {
			if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied && reply.Status != vfs.StatusBusy {
				t.Fatalf("operation %d on %s = %d", request.Operation, alias, reply.Status)
			}
		}
	}
	// What was refused left the content as it was.
	opened := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/system/bin/ls")})
	if reply := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64}); string(reply.Data) != "PROGRAM" {
		t.Fatalf("system program after refused changes = %q", reply.Data)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

func TestSystemDirectoryIsProtectedThroughEveryAlias(t *testing.T) {
	service := startedService(t, installedDevice(t))
	if !service.SystemProtected() {
		t.Fatal("installed system was not protected when the volume was mounted")
	}
	// FAT names ignore case, so every spelling reaches the one protected file.
	plain := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/system/bin/ls")})
	for _, alias := range []string{"/persist/SYSTEM/bin/ls", "/persist/sYsTeM/BIN/LS"} {
		if stat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte(alias)}); stat.Node != plain.Node {
			t.Fatalf("%s is node %d, /persist/system/bin/ls is %d", alias, stat.Node, plain.Node)
		}
	}
	refusesSystemChanges(t, service, "/persist/system", "/persist/SYSTEM", "/persist/sYsTeM")

	if status := service.Publish(); status != vfs.StatusOK {
		t.Fatalf("publish = %d", status)
	}
	refusesSystemChanges(t, service, "/system", "/persist/system")
	// The rest of the volume stays writable, including the published apps.
	writeFile(t, service, "/apps/tool", "APP")
	writeFile(t, service, "/home/user/note", "data")
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/apps/tool")}); stat.Status != vfs.StatusOK {
		t.Fatalf("stat of a published application through /persist = %d", stat.Status)
	}
	// A system program can be copied out and the copy changed freely.
	writeFile(t, service, "/home/user/bin-copy", "PROGRAM")
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/home/user/bin-copy")})
}

func TestProtectionSurvivesFilesystemRestartAndWithdraw(t *testing.T) {
	device := installedDevice(t)
	service := startedService(t, device)
	if service.Publish() != vfs.StatusOK {
		t.Fatal("publish failed")
	}
	// A restarted filesystem service serving the same volume keeps the
	// mounts and the protection.
	service.DetachFilesystem()
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("reattach failed")
	}
	refusesSystemChanges(t, service, "/system", "/persist/system")

	opened := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/system/bin/ls")})
	if status := service.Withdraw(); status != vfs.StatusBusy {
		t.Fatalf("withdraw with a file open through /system = %d", status)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if status := service.Withdraw(); status != vfs.StatusOK || service.Published() {
		t.Fatalf("withdraw = %d", status)
	}
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/system")}); stat.Status != vfs.StatusNotFound {
		t.Fatalf("stat /system after withdraw = %d", stat.Status)
	}
	// Withdrawing the mounts does not lift the protection of the volume.
	refusesSystemChanges(t, service, "/persist/system")
	// Nor can a client recreate /system or /apps in the volatile root as a
	// new alias, directly or by moving a directory there.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/tmp/fake")})
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationMkdir, Path: []byte("/system")},
		{Operation: vfs.OperationMkdir, Path: []byte("/tmp/../apps")},
		{Operation: vfs.OperationCreate, Path: []byte("/system/bin")},
		{Operation: vfs.OperationRename, Path: []byte("/tmp/fake"), Target: []byte("/system")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("operation %d on %s -> %s = %d", request.Operation, request.Path, request.Target, reply.Status)
		}
	}
	if status := service.Publish(); status != vfs.StatusOK {
		t.Fatalf("publish after withdraw = %d", status)
	}
	refusesSystemChanges(t, service, "/system")
}

func TestVolumeWithoutSystemIsNotPublished(t *testing.T) {
	service := startedService(t, testFAT32Device())
	if service.SystemProtected() {
		t.Fatal("a volume without a system directory reported one")
	}
	if status := service.Publish(); status != vfs.StatusNotFound {
		t.Fatalf("publish of a blank volume = %d", status)
	}
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/system")}); stat.Status != vfs.StatusNotFound {
		t.Fatalf("a system directory was created on a blank volume: %d", stat.Status)
	}
}

func TestProtectedFilesAreReportedReadOnly(t *testing.T) {
	service := startedService(t, installedDevice(t))
	for _, path := range []string{"/persist/system/bin/ls", "/persist/SYSTEM/commands", "/persist/system"} {
		stat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)})
		if !stat.AttributesAvailable || stat.Attributes&ramfs.AttrReadOnly == 0 {
			t.Fatalf("%s is not reported read-only: %+v", path, stat)
		}
	}
	opened := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/system/bin/ls")})
	if stat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Handle: opened.Handle}); stat.Attributes&ramfs.AttrReadOnly == 0 {
		t.Fatal("an open protected file is not reported read-only")
	}
	if stat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/home/user/ls")}); stat.Attributes&ramfs.AttrReadOnly != 0 {
		t.Fatal("a user file is reported read-only")
	}
}
