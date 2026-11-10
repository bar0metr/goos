package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ramfs"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

func TestDecodeOpen(t *testing.T) {
	cases := []struct {
		flags uint64
		want  OpenIntent
		errno abi.Errno
	}{
		{0, OpenIntent{Rights: vfs.Read}, 0},
		{1, OpenIntent{Rights: vfs.Write, Access: 1}, 0},
		{2, OpenIntent{Rights: vfs.Read | vfs.Write, Access: 2}, 0},
		{0o2000000 | 0o100000, OpenIntent{Rights: vfs.Read, CloseOnExec: true}, 0},
		{0o100 | 0o200 | 1, OpenIntent{Rights: vfs.Write, Access: 1, Create: true, Exclusive: true}, 0},
		{0o1000 | 0o100 | 1, OpenIntent{Rights: vfs.Write, Access: 1, Create: true, Truncate: true}, 0},
		{0o2000 | 0o100 | 1, OpenIntent{Rights: vfs.Write, Access: 1, Create: true, Append: true}, 0},
		{0o200000 | 0o2000000, OpenIntent{Rights: vfs.Read, Directory: true, CloseOnExec: true}, 0},
		{0o400 | 0o400000, OpenIntent{Rights: vfs.Read, NoFollow: true}, 0}, // O_NOCTTY, O_NOFOLLOW
		{1 << 32, OpenIntent{Rights: vfs.Read}, 0},                          // flags is an int
		{3, OpenIntent{}, abi.EINVAL},
		{0o200000 | 0o100, OpenIntent{}, abi.EINVAL},                                          // O_DIRECTORY|O_CREAT
		{0o4000 | 0o200000, OpenIntent{Rights: vfs.Read, Directory: true, NonBlock: true}, 0}, // opendir
		{0o10000, OpenIntent{}, abi.EOPNOTSUPP},                                               // O_DSYNC
		{0o40000, OpenIntent{}, abi.EOPNOTSUPP},                                               // O_DIRECT
		{0o10000000, OpenIntent{}, abi.EOPNOTSUPP},                                            // O_PATH
		{0o20200000 | 2, OpenIntent{}, abi.EOPNOTSUPP},                                        // O_TMPFILE
	}
	for _, test := range cases {
		got, errno := DecodeOpen(test.flags)
		if got != test.want || errno != test.errno {
			t.Errorf("flags %#o: %+v errno=%d, want %+v errno=%d", test.flags, got, errno, test.want, test.errno)
		}
	}
	if flags := (OpenIntent{Access: 1, Append: true}).StatusFlags(); flags != 1|abi.OpenAppend {
		t.Fatalf("status flags = %#o", flags)
	}
	for _, test := range []struct {
		intent OpenIntent
		opened vfs.Rights
	}{
		{OpenIntent{Rights: vfs.Read}, vfs.Read},
		{OpenIntent{Rights: vfs.Read, NoFollow: true}, vfs.Read | vfs.NoFollow},
		{OpenIntent{Rights: vfs.Write, Create: true}, vfs.Write},
		{OpenIntent{Rights: vfs.Write, Create: true, Exclusive: true}, vfs.Write},
		{OpenIntent{Rights: vfs.Write, Create: true, NoFollow: true}, vfs.Write | vfs.NoFollow},
	} {
		if opened := test.intent.OpenRights(); opened != test.opened {
			t.Errorf("%+v: open %#x", test.intent, opened)
		}
	}
}

func TestOpenIntentVFSFlags(t *testing.T) {
	for _, test := range []struct {
		intent   OpenIntent
		trailing bool
		want     vfs.OpenFlags
	}{
		{OpenIntent{}, false, 0},
		{OpenIntent{Create: true}, false, vfs.OpenCreate},
		{OpenIntent{Create: true, Exclusive: true}, false, vfs.OpenCreate | vfs.OpenExclusive},
		{OpenIntent{Exclusive: true}, false, 0},
		{OpenIntent{Truncate: true}, false, vfs.OpenTruncate},
		{OpenIntent{Directory: true}, true, vfs.OpenDirectory | vfs.OpenTrailingSlash},
	} {
		if got := test.intent.VFSOpenFlags(test.trailing); got != test.want {
			t.Errorf("%+v trailing=%v: %#x, want %#x", test.intent, test.trailing, got, test.want)
		}
	}
	if OpenStageOpen != 1 {
		t.Fatalf("open stage = %d", OpenStageOpen)
	}
}

func TestSeekTarget(t *testing.T) {
	const top = uint64(1)<<63 - 1
	cases := []struct {
		whence  uint64
		offset  int64
		current uint64
		size    uint64
		want    uint64
		errno   abi.Errno
	}{
		{abi.SeekSet, 0, 9, 100, 0, 0},
		{abi.SeekSet, 40, 9, 100, 40, 0},
		{abi.SeekSet, 400, 9, 100, 400, 0}, // past the end is a valid position
		{abi.SeekSet, -1, 9, 100, 0, abi.EINVAL},
		{abi.SeekCur, 0, 9, 100, 9, 0},
		{abi.SeekCur, -9, 9, 100, 0, 0},
		{abi.SeekCur, -10, 9, 100, 0, abi.EINVAL},
		{abi.SeekCur, 1, top, 100, 0, abi.EINVAL},
		{abi.SeekEnd, 0, 9, 100, 100, 0},
		{abi.SeekEnd, -100, 9, 100, 0, 0},
		{abi.SeekEnd, -101, 9, 100, 0, abi.EINVAL},
		{abi.SeekEnd, 5, 9, 100, 105, 0},
		{abi.SeekSet, int64(top), 0, 0, top, 0},
		{abi.SeekCur, -1 << 63, top, 0, 0, abi.EINVAL},
		{abi.SeekEnd, 0, 0, top + 1, 0, abi.EINVAL},
		{3, 0, 9, 100, 0, abi.EINVAL}, // SEEK_DATA
		{4, 0, 9, 100, 0, abi.EINVAL}, // SEEK_HOLE
	}
	for _, test := range cases {
		got, errno := SeekTarget(test.whence, test.offset, test.current, test.size)
		if got != test.want || errno != test.errno {
			t.Errorf("whence %d offset %d from %d/%d = %d errno=%d, want %d errno=%d", test.whence, test.offset, test.current, test.size, got, errno, test.want, test.errno)
		}
	}
}

func TestRemovalAndMutationErrno(t *testing.T) {
	cases := []struct {
		removeDirectory, trailing bool
		kind                      ramfs.Kind
		want                      abi.Errno
	}{
		{false, false, ramfs.KindFile, 0},
		{true, false, ramfs.KindDirectory, 0},
		{true, true, ramfs.KindDirectory, 0},
		{false, false, ramfs.KindDirectory, abi.EISDIR},
		{true, false, ramfs.KindFile, abi.ENOTDIR},
		{false, true, ramfs.KindFile, abi.ENOTDIR},
		{false, false, 0, abi.EIO},
	}
	for _, test := range cases {
		if got := RemovalErrno(test.removeDirectory, test.trailing, test.kind); got != test.want {
			t.Errorf("rmdir=%v trailing=%v kind=%d: errno %d, want %d", test.removeDirectory, test.trailing, test.kind, got, test.want)
		}
	}
	for status, want := range map[vfs.Status]abi.Errno{
		vfs.StatusOK: 0, vfs.StatusPermissionDenied: abi.EROFS, vfs.StatusResourceExhausted: abi.ENOSPC,
		vfs.StatusNoSpace: abi.ENOSPC, vfs.StatusNotEmpty: abi.ENOTEMPTY, vfs.StatusBusy: abi.EBUSY,
		vfs.StatusAlreadyExists: abi.EEXIST, vfs.StatusNotFound: abi.ENOENT, vfs.StatusIsDirectory: abi.EISDIR,
	} {
		if got := MutationErrno(status); got != want {
			t.Errorf("status %d: errno %d, want %d", status, got, want)
		}
	}
}

func TestDirectoryDescriptionsAreBoundedAndOwned(t *testing.T) {
	directorySlots = [MaxOpenDirectories]directorySlot{}
	first, ok := RetainDirectory(0x1000, []byte("/system/linux"))
	if !ok || first == 0 {
		t.Fatal("retain failed")
	}
	if path, found := DirectoryPath(0x1000, first); !found || string(path) != "/system/linux" {
		t.Fatalf("path = %q found=%v", path, found)
	}
	if _, found := DirectoryPath(0x2000, first); found {
		t.Fatal("another process read the description")
	}
	if ReleaseDirectory(0x2000, first) || !ReleaseDirectory(0x1000, first) || ReleaseDirectory(0x1000, first) {
		t.Fatal("release was not exact")
	}
	second, _ := RetainDirectory(0x1000, []byte("/tmp"))
	if _, found := DirectoryPath(0x1000, first); found || second == first {
		t.Fatal("a stale reference named the reused slot")
	}
	for _, bad := range []string{"", "relative"} {
		if _, ok := RetainDirectory(0x1000, []byte(bad)); ok {
			t.Fatalf("retained %q", bad)
		}
	}
	if _, ok := RetainDirectory(0, []byte("/")); ok {
		t.Fatal("retained for no owner")
	}
	held := 1
	for {
		if _, ok := RetainDirectory(0x3000, []byte("/")); !ok {
			break
		}
		held++
	}
	if held != MaxOpenDirectories {
		t.Fatalf("held %d descriptions, want %d", held, MaxOpenDirectories)
	}
	releaseDirectories(0x3000)
	if _, ok := RetainDirectory(0x4000, []byte("/")); !ok {
		t.Fatal("releasing a process freed nothing")
	}
	if _, found := DirectoryPath(0x1000, second); !found {
		t.Fatal("releasing one process dropped another's description")
	}
	directorySlots = [MaxOpenDirectories]directorySlot{}
}

func TestInstallDirectoryReleasesReferenceWhenAllocationFails(t *testing.T) {
	resetCompatibilityProcesses()
	defer resetCompatibilityProcesses()
	directorySlots = [MaxOpenDirectories]directorySlot{}
	const root = 0x5000
	if !InstallProcess(root, 60, linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4}), StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(root)
	if errno := state.FDs.Reserve(); errno != 0 {
		t.Fatalf("reserve: %d", errno)
	}
	for state.FDs.HasFree() {
		if _, errno := AllocateFD(root, FileDescription{Kind: FDVFSFile, Object: 1}, false); errno != 0 {
			t.Fatalf("fill descriptors: %d", errno)
		}
	}
	for i := 0; i < MaxOpenDirectories+1; i++ {
		fd, errno := InstallDirectory(root, state.FamilyID, []byte("/tmp"), OpenIntent{})
		if fd != -1 || errno != abi.EMFILE {
			t.Fatalf("attempt %d: fd=%d errno=%d", i, fd, errno)
		}
	}
	for i := range directorySlots {
		if directorySlots[i].owner != 0 {
			t.Fatalf("directory slot %d leaked", i)
		}
	}
	if directorySlots[0].generation != MaxOpenDirectories+1 {
		t.Fatalf("directory was not retained for each attempt: generation=%d", directorySlots[0].generation)
	}
	if _, _, errno := CloseFD(root, 3); errno != 0 {
		t.Fatalf("close: %d", errno)
	}
	fd, errno := InstallDirectory(root, state.FamilyID, []byte("/tmp"), OpenIntent{})
	if fd != 3 || errno != 0 {
		t.Fatalf("install after close: fd=%d errno=%d", fd, errno)
	}
	description, found := DescriptionOf(root, uint64(fd))
	if !found || description.Kind != FDVFSDirectory {
		t.Fatalf("directory description: %#v", description)
	}
	path, found := DirectoryPath(state.FamilyID, description.Object)
	if !found || string(path) != "/tmp" {
		t.Fatalf("directory path: %q found=%v", path, found)
	}
}

func TestProcessRemovalReleasesDirectoriesAndOffsetsAreShared(t *testing.T) {
	resetCompatibilityProcesses()
	directorySlots = [MaxOpenDirectories]directorySlot{}
	if !InstallProcess(0x5000, 60, linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4}), StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x5000)
	familyID := state.FamilyID
	reference, _ := RetainDirectory(familyID, []byte("/tmp"))
	directoryFD, _ := AllocateFD(0x5000, FileDescription{Kind: FDVFSDirectory, Object: reference}, false)
	fileFD, _ := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Flags: abi.OpenReadWrite, Object: 77}, false)
	duplicate, _ := state.FDs.Duplicate(fileFD, 0, false)
	description, found := DescriptionOf(0x5000, uint64(fileFD))
	if !found {
		t.Fatal("no description")
	}
	description.Offset = 4096
	if shared, _ := DescriptionOf(0x5000, uint64(duplicate)); shared != description || shared.Offset != 4096 {
		t.Fatal("duplicated descriptors do not share an offset")
	}
	if _, found := DescriptionOf(0x5000, LinuxFDCeiling); found {
		t.Fatal("out-of-range descriptor has a description")
	}
	if _, found := DescriptionOf(0x5000, uint64(directoryFD)+40); found {
		t.Fatal("closed descriptor has a description")
	}
	if !state.SetCWD([]byte("/tmp")) || string(state.CWD()) != "/tmp" || state.SetCWD([]byte("tmp")) || state.SetCWD(nil) {
		t.Fatalf("cwd = %q", state.CWD())
	}
	RemoveProcess(0x5000, nil)
	if _, found := DirectoryPath(familyID, reference); found {
		t.Fatal("process removal kept its directory description")
	}
}

func TestContinuationKeepsTwoPathsAndANote(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(5, 2)
	request := Request{Number: abi.SysRename, Arguments: [6]uint64{1, 2}}
	if table.Annotate(task, []byte("/b"), 7) {
		t.Fatal("annotated a call that never began")
	}
	if !table.Begin(task, request, []byte("/a")) || table.Annotate(task, []byte("/b\x00"), 7) || !table.Annotate(task, []byte("/b"), 7) {
		t.Fatal("annotation failed")
	}
	path, target := table.Paths(task)
	pending, _ := table.Pending(task)
	if string(path) != "/a" || string(target) != "/b" || pending.Note != 7 {
		t.Fatalf("paths %q %q note %d", path, target, pending.Note)
	}
	if !table.Issue(task, 1, 11) || table.Annotate(task, []byte("/c"), 8) {
		t.Fatal("annotation changed after the first request was issued")
	}
	if !table.Block(task, 1) || !table.Resume(task, 11, WakeMessage) || !table.Advance(task, 1, 3, 99) {
		t.Fatal("stage did not advance")
	}
	path, target = table.Paths(task)
	pending, _ = table.Pending(task)
	if string(path) != "/a" || string(target) != "/b" || pending.Note != 7 || pending.Partial != 99 || pending.Stage != 3 || pending.Issued {
		t.Fatalf("after advance: %q %q %+v", path, target, pending)
	}
	table.Finish(task)
	if path, target := table.Paths(task); path != nil || target != nil {
		t.Fatal("finished call kept its paths")
	}
}

func TestStatusFlags(t *testing.T) {
	file := FileDescription{Kind: FDVFSFile, Flags: abi.OpenReadWrite, Object: 1}
	cases := []struct {
		description FileDescription
		want        uint32
	}{
		{file, 0o100002},
		{FileDescription{Kind: FDVFSFile, Flags: abi.OpenWriteOnly | abi.OpenAppend, Object: 1}, 0o102001},
		{FileDescription{Kind: FDVFSDirectory, Object: 1}, 0o300000},
		{FileDescription{Kind: FDStreamReader, Object: 1}, 0},
		{FileDescription{Kind: FDStreamWriter, Object: 1}, 1},
	}
	for _, test := range cases {
		if got := StatusFlagsOf(test.description); got != test.want {
			t.Errorf("kind %d flags %#o: %#o, want %#o", test.description.Kind, test.description.Flags, got, test.want)
		}
	}
	if errno := SetStatusFlags(&file, uint64(abi.OpenAppend|abi.OpenReadOnly|abi.OpenCreate)); errno != 0 || file.Flags != abi.OpenReadWrite|abi.OpenAppend {
		t.Fatalf("set append: errno=%d flags=%#o", errno, file.Flags)
	}
	if errno := SetStatusFlags(&file, 0); errno != 0 || file.Flags != abi.OpenReadWrite {
		t.Fatalf("clear append: errno=%d flags=%#o", errno, file.Flags)
	}
	if errno := SetStatusFlags(&file, uint64(abi.OpenNonBlock)); errno != 0 || file.Flags != abi.OpenReadWrite|abi.OpenNonBlock {
		t.Fatalf("set nonblock: errno=%d flags=%#o", errno, file.Flags)
	}
	if errno := SetStatusFlags(&file, 0); errno != 0 || file.Flags != abi.OpenReadWrite {
		t.Fatalf("clear nonblock: errno=%d flags=%#o", errno, file.Flags)
	}
	for _, refused := range []uint32{abi.OpenAsync, abi.OpenDirect, abi.OpenNoATime} {
		if errno := SetStatusFlags(&file, uint64(refused|abi.OpenAppend)); errno != abi.EINVAL || file.Flags != abi.OpenReadWrite {
			t.Fatalf("flag %#o: errno=%d flags=%#o", refused, errno, file.Flags)
		}
	}
	stream := FileDescription{Kind: FDStreamWriter, Object: 1}
	if errno := SetStatusFlags(&stream, uint64(abi.OpenAppend)); errno != 0 || stream.Flags != 0 {
		t.Fatalf("append on a stream: errno=%d flags=%#o", errno, stream.Flags)
	}
	// A stream blocks, so it cannot claim not to.
	if errno := SetStatusFlags(&stream, uint64(abi.OpenNonBlock)); errno != abi.EINVAL || stream.Flags != 0 {
		t.Fatalf("nonblock on a stream: errno=%d flags=%#o", errno, stream.Flags)
	}
}
