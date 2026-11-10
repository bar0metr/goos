package vfsservice

import (
	"testing"

	"goos/userspace/fsproto"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// linksService mounts the links fixture, which Linux built with symbolic
// links of every shape, hard links, special files and owners, at /mnt/l
// beside the persistent FAT32 volume.
func linksService(t *testing.T) (*Service, *ext4Service) {
	t.Helper()
	service := startedService(t, installedDevice(t))
	return service, attachExt4(t, service, 1, "links", "/mnt/l")
}

func statPath(service *Service, owner vfs.Owner, path string, rights vfs.Rights) vfs.Reply {
	return service.Dispatch(owner, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path), Rights: rights})
}

func TestSymbolicLinksResolveInTheVFS(t *testing.T) {
	service, _ := linksService(t)
	for _, path := range []string{"/mnt/l/rel", "/mnt/l/dir/sub/up", "/mnt/l/link-\xfe", "/mnt/l/deep-1", "/mnt/l/hard-a"} {
		if text, status := readPath(t, service, path); status != vfs.StatusOK || text != "owned file\n" {
			t.Fatalf("%s: %q %v", path, text, status)
		}
	}
	if text, status := readPath(t, service, "/mnt/l/sub-dir/inner.txt"); status != vfs.StatusOK || text != "inner file\n" {
		t.Fatalf("through a directory link: %q %v", text, status)
	}
	for path, want := range map[string]vfs.Status{
		"/mnt/l/deep-0":     vfs.StatusLoop,
		"/mnt/l/loop-a":     vfs.StatusLoop,
		"/mnt/l/self":       vfs.StatusLoop,
		"/mnt/l/self/x":     vfs.StatusLoop,
		"/mnt/l/dangling":   vfs.StatusNotFound,
		"/mnt/l/bad-target": vfs.StatusNotFound,
		// An absolute target names the caller's namespace, and nothing is
		// at /file.txt there.
		"/mnt/l/abs": vfs.StatusNotFound,
	} {
		if _, status := readPath(t, service, path); status != want {
			t.Fatalf("%s: %v, want %v", path, status, want)
		}
	}
	// ".." in a target is applied to the spelled path: escape climbs to /etc,
	// the boot archive's directory, which is no file to open.
	if reply := statPath(service, testSession, "/mnt/l/escape", 0); reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindDirectory {
		t.Fatalf("escape: %+v", reply)
	}
	// A path that walks through a link the caller did not expand fails as
	// the backend would answer it.
	if _, _, status := service.namespace.Resolve([]byte("/mnt/l/sub-dir/inner.txt")); status != vfs.StatusNotDirectory {
		t.Fatalf("unexpanded walk through a link: %v", status)
	}
}

func TestLinkMetadataMatchesLinux(t *testing.T) {
	service, _ := linksService(t)
	file := statPath(service, testSession, "/mnt/l/file.txt", 0)
	hard := statPath(service, testSession, "/mnt/l/hard-b", 0)
	followed := statPath(service, testSession, "/mnt/l/rel", 0)
	if file.Status != vfs.StatusOK || hard.Node != file.Node || followed.Node != file.Node || file.Kind != ramfs.KindFile {
		t.Fatalf("file %+v, hard link %+v, followed %+v", file, hard, followed)
	}
	unix := file.Unix
	if !unix.Available || unix.Mode != 0x81a0 || unix.UID != 1234 || unix.GID != 5678 || unix.Links != 3 ||
		unix.Generation != 0x6f6f7367 || file.Node != 0x6f6f7367<<32|64 || unix.Inode != 64 || unix.Blocks != 8 ||
		!unix.Accessed.Available || unix.Accessed.Seconds != 1767225600 {
		t.Fatalf("file metadata %+v node %#x", unix, file.Node)
	}
	if sparse := statPath(service, testSession, "/mnt/l/sparse", 0); sparse.Size != 1048577 || sparse.Unix.Blocks != 8 || sparse.Unix.Inode != 71 {
		t.Fatalf("sparse file %+v", sparse)
	}
	link := statPath(service, testSession, "/mnt/l/rel", vfs.NoFollow)
	if link.Status != vfs.StatusOK || link.Kind != ramfs.KindSymlink || link.Size != 8 || link.Node == file.Node ||
		link.Unix.Mode != 0xa1ff || link.Unix.UID != 42 || link.Unix.GID != 43 {
		t.Fatalf("lstat of rel %+v", link)
	}
	directory := statPath(service, testSession, "/mnt/l/dir", 0)
	if directory.Unix.Mode != 0x41e8 || directory.Unix.UID != 7 || directory.Unix.Links != 3 || directory.Size != 4096 || directory.Unix.Inode != 58 {
		t.Fatalf("directory metadata %+v", directory.Unix)
	}
	if setuid := statPath(service, testSession, "/mnt/l/setuid", 0); setuid.Unix.Mode != 0x89ed {
		t.Fatalf("setuid mode %#x", setuid.Unix.Mode)
	}
	for path, want := range map[string]struct {
		mode   uint16
		device uint32
	}{
		"/mnt/l/fifo":  {0x11a4, 0},
		"/mnt/l/char":  {0x21a4, 1<<8 | 3},
		"/mnt/l/block": {0x61a4, 8<<8 | 1},
		// major 259 and minor 300 need the wide encoding.
		"/mnt/l/wide": {0x21a4, 0x2c | 259<<8 | 0x100<<12},
	} {
		reply := statPath(service, testSession, path, 0)
		if reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindSpecial || reply.Unix.Mode != want.mode || reply.Unix.Device != want.device {
			t.Fatalf("%s: %+v", path, reply)
		}
		if _, status := readPath(t, service, path); status != vfs.StatusUnsupported {
			t.Fatalf("open %s: %v", path, status)
		}
	}
	// FAT32 keeps no Unix metadata and claims none.
	writeFile(t, service, "/home/user/plain", "x")
	if plain := statPath(service, testSession, "/home/user/plain", 0); plain.Status != vfs.StatusOK || plain.Unix.Available {
		t.Fatalf("FAT32 stat %+v", plain)
	}
}

func readLink(service *Service, owner vfs.Owner, path string) (string, vfs.Status) {
	reply := service.Dispatch(owner, vfs.Request{Operation: vfs.OperationReadLink, Path: []byte(path)})
	return string(reply.Name), reply.Status
}

func TestReadLinkAndNoFollow(t *testing.T) {
	service, _ := linksService(t)
	for path, want := range map[string]string{
		"/mnt/l/rel":            "file.txt",
		"/mnt/l/abs":            "/file.txt",
		"/mnt/l/bad-target":     "bad-\xff",
		"/mnt/l/sub-dir/../rel": "file.txt",
		"/mnt/l/dir/sub/up":     "../../file.txt",
		// The directory part of the path is followed; only the last
		// component is read.
		"/mnt/l/sub-dir/up": "../../file.txt",
	} {
		if target, status := readLink(service, testSession, path); status != vfs.StatusOK || target != want {
			t.Fatalf("readlink %s: %q %v", path, target, status)
		}
	}
	if _, status := readLink(service, testSession, "/mnt/l/file.txt"); status != vfs.StatusInvalidArgument {
		t.Fatalf("readlink of a file: %v", status)
	}
	if _, status := readLink(service, testSession, "/home/user"); status != vfs.StatusInvalidArgument {
		t.Fatalf("readlink on FAT32: %v", status)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.NoFollow, Path: []byte("/mnt/l/rel")})
	if opened.Status != vfs.StatusLoop {
		t.Fatalf("no-follow open of a link: %v", opened.Status)
	}
	opened = service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.NoFollow, Path: []byte("/mnt/l/sub-dir/inner.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("no-follow leaves directory links followed: %v", opened.Status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

func TestLinksOnReadOnlyAndAcrossVolumes(t *testing.T) {
	service, _ := linksService(t)
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationSymlink, Path: []byte("/mnt/l/new"), Target: []byte("file.txt")},
		{Operation: vfs.OperationLink, Path: []byte("/mnt/l/file.txt"), Target: []byte("/mnt/l/new")},
		{Operation: vfs.OperationUnlink, Path: []byte("/mnt/l/rel")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("operation %d on the read-only volume: %v", request.Operation, reply.Status)
		}
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationLink, Path: []byte("/mnt/l/file.txt"), Target: []byte("/tmp/alias")}); reply.Status != vfs.StatusCrossDevice {
		t.Fatalf("link across volumes: %v", reply.Status)
	}
	writeFile(t, service, "/home/user/f", "x")
	// /home and /persist are one FAT32 instance; it keeps no hard links.
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationLink, Path: []byte("/home/user/f"), Target: []byte("/persist/home/user/g")}); reply.Status != vfs.StatusUnsupported {
		t.Fatalf("FAT32 hard link: %v", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/home/user/s"), Target: []byte("f")}); reply.Status != vfs.StatusUnsupported {
		t.Fatalf("FAT32 symlink: %v", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/bootstrap/s"), Target: []byte("f")}); reply.Status != vfs.StatusPermissionDenied {
		t.Fatalf("symlink on the boot archive: %v", reply.Status)
	}
}

func TestRAMFSLinksCrossMounts(t *testing.T) {
	service, _ := linksService(t)
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/to-ext4"), Target: []byte("/mnt/l/sub-dir")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/chain"), Target: []byte("to-ext4/up")})
	if text, status := readPath(t, service, "/tmp/chain"); status != vfs.StatusOK || text != "owned file\n" {
		t.Fatalf("RAMFS link into ext4 and back: %q %v", text, status)
	}
	if target, status := readLink(service, testSession, "/tmp/chain"); status != vfs.StatusOK || target != "to-ext4/up" {
		t.Fatalf("readlink %q %v", target, status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/chain"), Target: []byte("x")}); reply.Status != vfs.StatusAlreadyExists {
		t.Fatalf("symlink over a name: %v", reply.Status)
	}
	// Create follows a dangling link and makes its target, as open(O_CREAT)
	// does on Linux; mkdir finds the name taken.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/dangling"), Target: []byte("/tmp/made")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/dangling")})
	if reply := statPath(service, testSession, "/tmp/made", 0); reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindFile {
		t.Fatalf("dangling target not made: %v", reply.Status)
	}
	if reply := statPath(service, testSession, "/tmp/dangling", vfs.NoFollow); reply.Kind != ramfs.KindSymlink {
		t.Fatal("the link itself changed")
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/tmp/dangling")}); reply.Status != vfs.StatusAlreadyExists {
		t.Fatalf("mkdir over a link: %v", reply.Status)
	}
	// Removing and renaming act on the link.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/tmp/to-ext4"), Target: []byte("/tmp/moved")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/tmp/moved")})
	if text, status := readPath(t, service, "/mnt/l/sub-dir/inner.txt"); status != vfs.StatusOK || text != "inner file\n" {
		t.Fatalf("target changed with its link: %q %v", text, status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationLink, Rights: vfs.NoFollow, Path: []byte("/tmp/chain"), Target: []byte("/tmp/hard")}); reply.Status != vfs.StatusUnsupported {
		t.Fatalf("RAMFS hard link: %v", reply.Status)
	}
	long := make([]byte, ramfs.MaxLinkTarget+1)
	for index := range long {
		long[index] = 'a'
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/long"), Target: long}); reply.Status != vfs.StatusNameTooLong {
		t.Fatalf("overlong target: %v", reply.Status)
	}
}

func TestLinksCannotReachProtectedSystem(t *testing.T) {
	service, _ := linksService(t)
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/sys"), Target: []byte("/persist/SYSTEM")})
	if text, status := readPath(t, service, "/tmp/sys/bin/ls"); status != vfs.StatusOK || text != "PROGRAM" {
		t.Fatalf("read through a link to the system: %q %v", text, status)
	}
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationCreate, Path: []byte("/tmp/sys/new")},
		{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/tmp/sys/bin/ls")},
		{Operation: vfs.OperationUnlink, Path: []byte("/tmp/sys/commands")},
		{Operation: vfs.OperationTruncate, Path: []byte("/tmp/sys/bin/ls")},
		{Operation: vfs.OperationRename, Path: []byte("/tmp/sys/commands"), Target: []byte("/tmp/stolen")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("operation %d through a link to the system: %v", request.Operation, reply.Status)
		}
	}
	// The link itself lives in /tmp and may go.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/tmp/sys")})
}

func TestRootedSessionsStayInsideThroughLinks(t *testing.T) {
	service, _ := linksService(t)
	const jailed vfs.Owner = 78
	authority, ok := vfs.RootedAuthority([]byte("/mnt/l"))
	if !ok || service.SetAuthority(jailed, authority) != vfs.StatusOK {
		t.Fatal("authority")
	}
	// An absolute target starts at the session's root.
	if text, status := readPath(t, service, "/mnt/l/abs"); status != vfs.StatusNotFound {
		t.Fatalf("unrooted abs: %q %v", text, status)
	}
	opened := service.Dispatch(jailed, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/l/abs")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("rooted abs: %v", opened.Status)
	}
	service.Dispatch(jailed, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	deny(t, service, jailed,
		vfs.Request{Operation: vfs.OperationStat, Path: []byte("/mnt/l/escape")},
		vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/mnt/l/escape")},
		vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/l/escape/hosts")})
	// The link itself is inside the root and may be described.
	if reply := statPath(service, jailed, "/mnt/l/escape", vfs.NoFollow); reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindSymlink {
		t.Fatalf("lstat of escape: %+v", reply)
	}
	if target, status := readLink(service, jailed, "/mnt/l/escape"); status != vfs.StatusOK || target != "../../../../../../etc" {
		t.Fatalf("readlink escape %q %v", target, status)
	}
	// A dangling link inside the root cannot create outside it.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/tmp/jail")})
	jail, _ := vfs.RootedAuthority([]byte("/tmp/jail"))
	const jailedTmp vfs.Owner = 79
	if service.SetAuthority(jailedTmp, jail) != vfs.StatusOK {
		t.Fatal("authority")
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/jail/out"), Target: []byte("../escaped")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/jail/in"), Target: []byte("/made")})
	deny(t, service, jailedTmp, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, OpenFlags: vfs.OpenCreate, Path: []byte("/tmp/jail/out")})
	if reply := statPath(service, testSession, "/tmp/escaped", 0); reply.Status != vfs.StatusNotFound {
		t.Fatalf("a rooted create escaped: %v", reply.Status)
	}
	created := service.Dispatch(jailedTmp, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, OpenFlags: vfs.OpenCreate, Path: []byte("/tmp/jail/in")})
	if created.Status != vfs.StatusOK {
		t.Fatalf("rooted create through an absolute link: %v", created.Status)
	}
	service.Dispatch(jailedTmp, vfs.Request{Operation: vfs.OperationClose, Handle: created.Handle})
	if reply := statPath(service, testSession, "/tmp/jail/made", 0); reply.Status != vfs.StatusOK {
		t.Fatalf("absolute target outside the root: %v", reply.Status)
	}
	// A link elsewhere into the root does not let a rooted session out.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/outside"), Target: []byte("/mnt/l/file.txt")})
	deny(t, service, jailed, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/tmp/outside")})
}

func TestScopesApplyToLinkTargets(t *testing.T) {
	service, _ := protectedRemote(t)
	attachExt4(t, service, 1, "links", "/mnt/l")
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/key"), Target: []byte("/state/security/ssh/host")})
	deny(t, service, remoteOwner,
		vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/tmp/key")},
		vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, OpenFlags: vfs.OpenCreate, Path: []byte("/tmp/key")},
		vfs.Request{Operation: vfs.OperationStat, Path: []byte("/tmp/key")})
}

func TestStaleHandlesAfterRestartWithLinks(t *testing.T) {
	service, instance := linksService(t)
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/l/rel")})
	if opened.Status != vfs.StatusOK {
		t.Fatal(opened.Status)
	}
	instance.fail = true
	service.VolumeOffline(1)
	if _, status := readLink(service, testSession, "/mnt/l/rel"); status == vfs.StatusOK {
		t.Fatal("offline volume answered a readlink")
	}
	replacement := newExt4Service(t, "links")
	if status := service.AttachVolume(1, replacement, replacement.cache, []byte("/mnt/l")); status != vfs.StatusOK {
		t.Fatalf("reattach: %v", status)
	}
	if read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 16}); read.Status != vfs.StatusNotFound {
		t.Fatalf("old handle after restart: %v", read.Status)
	}
	if text, status := readPath(t, service, "/mnt/l/rel"); status != vfs.StatusOK || text != "owned file\n" {
		t.Fatalf("reopen after restart: %q %v", text, status)
	}
}

func TestNoFollowMetadataChangesTheLink(t *testing.T) {
	service, _ := linksService(t)
	writeFile(t, service, "/tmp/target", "x")
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/link"), Target: []byte("target")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/tmp/gone"), Target: []byte("missing")})
	stamp := func(seconds int64) ramfs.MetadataChange {
		return ramfs.MetadataChange{SetModified: true, Modified: ramfs.Timestamp{Seconds: seconds, Available: true}}
	}
	change := func(path string, rights vfs.Rights, seconds int64) vfs.Status {
		request := vfs.MetadataRequest([]byte(path), 0, stamp(seconds))
		request.Rights |= rights
		return service.Dispatch(testSession, request).Status
	}
	if change("/tmp/link", 0, 100) != vfs.StatusOK || change("/tmp/link", vfs.NoFollow, 200) != vfs.StatusOK {
		t.Fatal("metadata change")
	}
	if target := statPath(service, testSession, "/tmp/target", 0); target.Modified.Seconds != 100 {
		t.Fatalf("followed change set the target to %d", target.Modified.Seconds)
	}
	if link := statPath(service, testSession, "/tmp/link", vfs.NoFollow); link.Modified.Seconds != 200 {
		t.Fatalf("no-follow change set the link to %d", link.Modified.Seconds)
	}
	if change("/tmp/gone", vfs.NoFollow, 300) != vfs.StatusOK || change("/tmp/gone", 0, 300) != vfs.StatusNotFound {
		t.Fatal("dangling link")
	}
	before := statPath(service, testSession, "/mnt/l/file.txt", 0)
	if status := change("/mnt/l/rel", vfs.NoFollow, 400); status != vfs.StatusPermissionDenied {
		t.Fatalf("ext4 link: %v", status)
	}
	if after := statPath(service, testSession, "/mnt/l/file.txt", 0); after.Modified != before.Modified {
		t.Fatal("ext4 target changed")
	}
}

func TestCrossFilesystemRenameIsCrossDevice(t *testing.T) {
	service, _ := linksService(t)
	attachExt4(t, service, 2, "canonical", "/mnt/c")
	writeFile(t, service, "/tmp/file", "x")
	writeFile(t, service, "/home/user/old", "old")
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationRename, Path: []byte("/mnt/l/file.txt"), Target: []byte("/mnt/c/moved")},
		{Operation: vfs.OperationRename, Path: []byte("/tmp/file"), Target: []byte("/home/user/new")},
		{Operation: vfs.OperationReplace, Path: []byte("/tmp/file"), Target: []byte("/home/user/old")},
		{Operation: vfs.OperationReplace, Path: []byte("/tmp/file"), Target: []byte("/mnt/c/hello.txt")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusCrossDevice {
			t.Fatalf("%s -> %s: %v", request.Path, request.Target, reply.Status)
		}
	}
	// /home and /persist/home are one FAT32 instance.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/home/user/old"), Target: []byte("/persist/home/user/renamed")})
	if text, status := readPath(t, service, "/home/user/renamed"); status != vfs.StatusOK || text != "old" {
		t.Fatalf("rename between aliases: %q %v", text, status)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/tmp/file"), Target: []byte("/tmp/same")})
}

// flushCounter counts the flushes a filesystem service receives.
type flushCounter struct {
	inner   Transport
	flushes int
}

func (counter *flushCounter) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if request.Operation == fsproto.OperationFlush {
		counter.flushes++
	}
	return counter.inner.Exchange(request)
}

func TestSyncReachesOnlyTheOwningFilesystem(t *testing.T) {
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
		t.Fatal("service init failed")
	}
	persistent := &flushCounter{inner: newLoopback(installedDevice(t))}
	if !service.AttachFilesystem(persistent) {
		t.Fatal("persistent mount failed")
	}
	instance := newExt4Service(t, "links")
	volume := &flushCounter{inner: instance}
	if status := service.AttachVolume(1, volume, instance.cache, []byte("/mnt/l")); status != vfs.StatusOK {
		t.Fatal(status)
	}
	writeFile(t, service, "/home/user/f", "x")
	writeFile(t, service, "/tmp/f", "x")
	sync := func(path string, directory bool) vfs.Status {
		if directory {
			return service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSync, Path: []byte(path)}).Status
		}
		opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
		if opened.Status != vfs.StatusOK {
			return opened.Status
		}
		defer service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
		return service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSync, Handle: opened.Handle}).Status
	}
	for _, step := range []struct {
		path      string
		directory bool
		fat, ext4 int
	}{
		{"/mnt/l/rel", false, 0, 1},
		{"/mnt/l/dir", true, 0, 2},
		{"/home/user/f", false, 1, 2},
		{"/home/user", true, 2, 2},
		{"/tmp/f", false, 2, 2},
		{"/tmp", true, 2, 2},
		{"/bootstrap", true, 2, 2},
	} {
		if status := sync(step.path, step.directory); status != vfs.StatusOK {
			t.Fatalf("sync %s: %v", step.path, status)
		}
		if persistent.flushes != step.fat || volume.flushes != step.ext4 {
			t.Fatalf("after %s: FAT32 %d flushes, ext4 %d", step.path, persistent.flushes, volume.flushes)
		}
	}
	instance.fail = true
	if status := sync("/mnt/l/dir", true); status == vfs.StatusOK {
		t.Fatal("sync of a dead volume succeeded")
	}
	if persistent.flushes != 2 {
		t.Fatal("a dead volume's sync reached FAT32")
	}
}

func TestListingCarriesInodeNumbers(t *testing.T) {
	service, _ := linksService(t)
	target := make([]byte, vfs.MaxDirBatch)
	inodes := map[string]uint64{}
	cursor := uint64(0)
	for {
		reply := service.ReadDirBatch(testSession, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/mnt/l"), Offset: cursor, NameReturn: true}, target)
		if reply.Status != vfs.StatusOK {
			t.Fatal(reply.Status)
		}
		var record ramfs.DirRecord
		for data := target[:reply.Size]; len(data) != 0; {
			size, ok := record.Decode(data)
			if !ok {
				t.Fatal("malformed record")
			}
			inodes[string(record.Name)] = record.Inode
			data = data[size:]
		}
		if reply.BatchEnd {
			break
		}
		cursor = uint64(reply.Cursor)
	}
	if inodes["file.txt"] != 64 || inodes["hard-a"] != 64 || inodes["dir"] != 58 || inodes["sparse"] != 71 {
		t.Fatalf("listed inode numbers %v", inodes)
	}
}

// TestCreateRaceWithALink preserves the old adversarial setup, but the Linux
// side now sends one open intent after the other session inserts the link.
func TestCreateRaceWithALink(t *testing.T) {
	service, _ := linksService(t)
	const other vfs.Owner = 80
	for _, test := range []struct {
		name      string
		rights    vfs.Rights
		openFlags vfs.OpenFlags
		status    vfs.Status
	}{
		{"exclusive", vfs.Write, vfs.OpenCreate | vfs.OpenExclusive, vfs.StatusAlreadyExists},
		{"no-follow", vfs.Write | vfs.NoFollow, vfs.OpenCreate, vfs.StatusLoop},
		{"plain", vfs.Write, vfs.OpenCreate, vfs.StatusOK},
	} {
		path, victim := "/tmp/race-"+test.name, "/tmp/victim-"+test.name
		if reply := statPath(service, testSession, path, vfs.NoFollow); reply.Status != vfs.StatusNotFound {
			t.Fatalf("%s: first stat %v", test.name, reply.Status)
		}
		if reply := service.Dispatch(other, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte(path), Target: []byte(victim)}); reply.Status != vfs.StatusOK {
			t.Fatalf("%s: the other session's link: %v", test.name, reply.Status)
		}
		opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: test.rights, OpenFlags: test.openFlags, Path: []byte(path)})
		if opened.Status != test.status {
			t.Fatalf("%s: atomic open: %v", test.name, opened.Status)
		}
		if test.status != vfs.StatusOK {
			if reply := statPath(service, testSession, victim, 0); reply.Status != vfs.StatusNotFound {
				t.Fatalf("%s: the link's target was made: %v", test.name, reply.Status)
			}
			continue
		}
		target := statPath(service, testSession, victim, 0)
		byHandle := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Handle: opened.Handle})
		if target.Status != vfs.StatusOK || byHandle.Status != vfs.StatusOK || target.Node != byHandle.Node {
			t.Fatalf("%s: target=%+v handle=%+v", test.name, target, byHandle)
		}
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	}
	message, ok := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationCreate, Rights: vfs.NoFollow, Path: []byte("/tmp/x")}, 9)
	if decoded, valid := vfs.DecodeRequest(message); !ok || !valid || decoded.Rights != vfs.NoFollow {
		t.Fatal("NoFollow on a create did not cross the wire")
	}
}
