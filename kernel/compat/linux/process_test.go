package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/userspace/startup"
)

func TestProcessStateRetainsLaunchAuthority(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/persist/work", [4]handle.ID{11, 12, 13, 14})
	streams := StandardStreams{Input: 11, Output: 12, Error: 13}
	if !InstallProcess(0x4000, 10, data, streams, 0x600000, 0x4600000) || InstallProcess(0x4000, 11, data, streams, 0x600000, 0x4600000) {
		t.Fatal("state install did not enforce unique root")
	}
	state, found := FindProcess(0x4000)
	if !found || state.VFSHandle != 14 || state.PID < firstPID ||
		state.BrkBase != 0x600000 || state.BrkCurrent != 0x600000 || state.BrkLimit != 0x4600000 || string(state.CWD()) != "/persist/work" {
		t.Fatalf("state = %#v cwd=%q", state, state.CWD())
	}
	for fd := 0; fd <= 2; fd++ {
		if _, found := LookupFD(0x4000, fd); !found {
			t.Fatalf("startup fd %d missing", fd)
		}
	}
	record, found := FindProcessRecord(state.PID)
	if !found || record.GoOSProcessID != 10 || record.ParentPID != 1 || record.FamilyID == 0 || record.State != ProcessRunning {
		t.Fatalf("record = %#v", record)
	}
	pid := state.PID
	if !RemoveProcess(0x4000, nil) {
		t.Fatal("state removal failed")
	}
	if _, found := FindProcess(0x4000); found || RemoveProcess(0x4000, nil) {
		t.Fatal("removed state remained visible")
	}
	if _, found := FindProcessRecord(pid); found {
		t.Fatal("removed process kept its compatibility identity")
	}
}

func TestProcessStateIsBounded(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	for index := range processStates {
		if !InstallProcess(uint64(index+1), process.ID(index+1), data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
			t.Fatalf("install %d failed", index)
		}
	}
	if InstallProcess(uint64(len(processStates)+1), process.ID(len(processStates)+1), data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("over-capacity state installed")
	}
}

func TestProcessFDExhaustionIsEMFILE(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x5000, 20, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	for fd := 3; fd < LinuxMaxFD; fd++ {
		allocated, errno := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: uint64(fd + 100)}, false)
		if errno != 0 || allocated != fd {
			t.Fatalf("fd %d = %d errno=%d", fd, allocated, errno)
		}
	}
	if fd, errno := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: 999}, false); fd != -1 || errno != abi.EMFILE {
		t.Fatalf("full table = fd %d errno %d", fd, errno)
	}
}

func TestExecReplacementPreservesStateAndClosesMarkedDescriptors(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/persist/work", [4]handle.ID{11, 12, 13, 14})
	if !InstallProcess(0x9000, 60, data, StandardStreams{Input: 11, Output: 12, Error: 13}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x9000)
	state.Signals.Mask(abi.SignalSetMask, 7, true)
	closed, errno := AllocateFD(0x9000, FileDescription{Kind: FDVFSFile, Object: 71}, true)
	kept, errno2 := AllocateFD(0x9000, FileDescription{Kind: FDVFSFile, Object: 72, Offset: 19}, false)
	if errno != 0 || errno2 != 0 || !ReplaceRoot(0x9000, 0xa000, 0x800000, 0x4800000) {
		t.Fatalf("fd=%d,%d errno=%d,%d", closed, kept, errno, errno2)
	}
	var handles [LinuxMaxFD]uint64
	if count := state.CloseOnExecVFS(handles[:]); count != 1 || handles[0] != 71 {
		t.Fatalf("close handles count=%d handles=%v", count, handles[:count])
	}
	released := FileDescription{}
	state.CloseOnExec(func(description FileDescription) { released = description })
	replaced, found := FindProcess(0xa000)
	blocked, _ := replaced.Signals.Mask(0, 0, false)
	if !found || replaced.PID != state.PID || replaced.FamilyID != state.FamilyID || string(replaced.CWD()) != "/persist/work" || blocked != 7 {
		t.Fatalf("replaced state = %#v", replaced)
	}
	if _, found := replaced.FDs.Lookup(closed); found || released.Object != 71 {
		t.Fatalf("closed fd remained or released=%#v", released)
	}
	if description, found := replaced.FDs.Lookup(kept); !found || description.Object != 72 || description.Offset != 19 {
		t.Fatalf("retained fd = %#v found=%v", description, found)
	}
	resetCompatibilityProcesses()
}

func linuxStartup(t *testing.T, cwd string, ids [4]handle.ID) startup.Data {
	t.Helper()
	var target [startup.MaxSize]byte
	writer := startup.NewWriter(target[:], 1, 0, 4)
	writer.Command([]byte("/system/linux/probe"))
	writer.CWD([]byte(cwd))
	writer.Argument([]byte("/system/linux/probe"))
	for _, name := range []string{"in", "stdout", "stderr", "vfs"} {
		writer.Handle([]byte(name), 0)
	}
	template, ok := writer.Finish()
	if !ok || !startup.BindHandles(template, ids[:]) {
		t.Fatal("startup construction failed")
	}
	data, ok := startup.Decode(template)
	if !ok {
		t.Fatal("startup decode failed")
	}
	return data
}

func TestCompatibilityPIDsAreDistinctAndBounded(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	nextPID = MaxPID
	streams := StandardStreams{Input: 1, Output: 2, Error: 3}
	if !InstallProcess(0x1000, 30, data, streams, 0x600000, 0x4600000) || !InstallProcess(0x2000, 31, data, streams, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	first, _ := FindProcess(0x1000)
	second, _ := FindProcess(0x2000)
	if first.PID != MaxPID || second.PID != firstPID {
		t.Fatalf("pids = %d,%d; the range did not wrap", first.PID, second.PID)
	}
	// The counter returns to a live PID and must step past it.
	nextPID = MaxPID
	if !InstallProcess(0x3000, 32, data, streams, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	if third, _ := FindProcess(0x3000); third.PID != firstPID+1 {
		t.Fatalf("pid = %d, want the first free value", third.PID)
	}
	resetCompatibilityProcesses()
}

func TestCloseFDReportsTheLastReference(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x6000, 40, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x6000)
	fd, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 77}, true)
	duplicate, duplicated := state.FDs.Duplicate(fd, 0, false)
	if errno != 0 || fd != 3 || !duplicated || duplicate != 4 || !state.FDs.HasFree() {
		t.Fatalf("fd=%d errno=%d duplicate=%d", fd, errno, duplicate)
	}
	if _, last, errno := CloseFD(0x6000, fd); errno != 0 || last {
		t.Fatalf("first close: last=%v errno=%d", last, errno)
	}
	released, last, errno := CloseFD(0x6000, duplicate)
	if errno != 0 || !last || released.Kind != FDVFSFile || released.Object != 77 {
		t.Fatalf("final close: %#v last=%v errno=%d", released, last, errno)
	}
	if _, _, errno := CloseFD(0x6000, duplicate); errno != abi.EBADF {
		t.Fatalf("stale fd errno = %d", errno)
	}
	if _, _, errno := CloseFD(0x7000, 0); errno != abi.EBADF {
		t.Fatalf("unknown process errno = %d", errno)
	}
	for state.FDs.HasFree() {
		if _, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 1}, false); errno != 0 {
			t.Fatalf("allocate errno = %d", errno)
		}
	}
	if _, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 1}, false); errno != abi.EMFILE {
		t.Fatalf("full table errno = %d", errno)
	}
	resetCompatibilityProcesses()
}

func TestWorkingDirectoryIsStoredCanonical(t *testing.T) {
	resetCompatibilityProcesses()
	if !InstallProcess(0x8000, 50, linuxStartup(t, "/persist//work/./deep/..", [4]handle.ID{1, 2, 3, 4}), StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	if state, _ := FindProcess(0x8000); string(state.CWD()) != "/persist/work" {
		t.Fatalf("cwd = %q", state.CWD())
	}
	resetCompatibilityProcesses()
}

func TestProcessFamilyLifecycle(t *testing.T) {
	resetCompatibilityProcesses()
	root, ok := installProcessRecord(100, 1, 0)
	if !ok {
		t.Fatal("root registration failed")
	}
	child, ok := registerChildForTest(101, root.PID)
	if !ok {
		t.Fatal("child registration failed")
	}
	grandchild, ok := registerChildForTest(102, child.PID)
	if !ok || child.FamilyID != root.FamilyID || grandchild.FamilyID != root.FamilyID || ParentPIDOf(grandchild.PID) != child.PID {
		t.Fatalf("child=%#v grandchild=%#v", child, grandchild)
	}
	if !markProcessExited(child.PID, Termination{Status: 17}) {
		t.Fatal("child exit failed")
	}
	zombie, found := FindProcessRecord(child.PID)
	if !found || zombie.State != ProcessZombie || zombie.ExitStatus != 17 || ParentPIDOf(grandchild.PID) != 1 {
		t.Fatalf("zombie=%#v grandchild ppid=%d", zombie, ParentPIDOf(grandchild.PID))
	}
	nextPID = child.PID
	replacement, ok := registerChildForTest(103, root.PID)
	if !ok || replacement.PID == child.PID {
		t.Fatalf("replacement = %#v", replacement)
	}
	reaped, ok := ReapChild(root.PID, child.PID)
	if !ok || reaped.State != ProcessReaped || reaped.ExitStatus != 17 {
		t.Fatalf("reaped = %#v", reaped)
	}
	nextPID = child.PID
	reused, ok := registerChildForTest(104, root.PID)
	if !ok || reused.PID != child.PID {
		t.Fatalf("reused = %#v", reused)
	}
}

func TestOrphanZombiesAreReaped(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(200, 1, 0)
	zombie, _ := registerChildForTest(201, root.PID)
	running, _ := registerChildForTest(202, root.PID)
	if !markProcessExited(zombie.PID, Termination{Signal: 9}) {
		t.Fatal("child exit transition failed")
	}
	exited, found := FindProcessRecord(zombie.PID)
	if !found || exited.SignalTermination != 9 {
		t.Fatalf("exited child = %#v", exited)
	}
	if !markProcessExited(root.PID, Termination{}) {
		t.Fatal("exit transition failed")
	}
	if _, found := FindProcessRecord(root.PID); found {
		t.Fatal("launch root remained as a zombie")
	}
	if _, found := FindProcessRecord(zombie.PID); found {
		t.Fatal("orphan zombie remained reserved")
	}
	orphan, found := FindProcessRecord(running.PID)
	if !found || orphan.ParentPID != 1 || orphan.State != ProcessRunning {
		t.Fatalf("orphan = %#v", orphan)
	}
	nextPID = root.PID
	newRoot, ok := installProcessRecord(203, 1, 0)
	if !ok || newRoot.PID != root.PID || newRoot.FamilyID == orphan.FamilyID {
		t.Fatalf("new root = %#v orphan = %#v", newRoot, orphan)
	}
	if !markProcessExited(orphan.PID, Termination{Signal: 15}) {
		t.Fatal("orphan exit failed")
	}
	if _, found := FindProcessRecord(orphan.PID); found {
		t.Fatal("orphan exit was not reaped")
	}
}

func TestProcessFamilyRejectsInvalidRelations(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(300, 1, 0)
	if _, status := RegisterChild(300, root.PID, 1); status != ChildRegistrationInvalid {
		t.Fatal("duplicate GoOS process identity accepted")
	}
	if _, status := RegisterChild(301, MaxPID, 1); status != ChildRegistrationInvalid {
		t.Fatal("missing parent accepted")
	}
	reserved, ok := registerChildForTest(301, root.PID)
	if !ok || !RollbackProcessRecord(301) {
		t.Fatal("child reservation rollback failed")
	}
	if _, found := FindProcessRecord(reserved.PID); found || RollbackProcessRecord(301) {
		t.Fatal("rolled-back child remained registered")
	}
	if _, ok := ReapChild(root.PID, root.PID); ok {
		t.Fatal("running process reaped")
	}
	if ParentPIDOf(MaxPID) != 1 {
		t.Fatal("unknown process did not use init parent")
	}
}

func TestChildRegistrationEnforcesFamilyAndNativeReserve(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(500, 1, 0)
	children := make([]ProcessRecord, 0, LinuxFamilyMaxProcesses-1)
	for index := 0; index < LinuxFamilyMaxProcesses-1; index++ {
		child, status := RegisterChild(process.ID(501+index), root.PID, 1+index)
		if status != ChildRegistrationOK {
			t.Fatalf("child %d status = %d", index, status)
		}
		children = append(children, child)
	}
	if _, status := RegisterChild(700, root.PID, LinuxFamilyMaxProcesses); status != ChildRegistrationLimit {
		t.Fatalf("family overflow status = %d", status)
	}
	if !markProcessExited(children[0].PID, Termination{}) {
		t.Fatal("child exit failed")
	}
	if _, status := RegisterChild(701, root.PID, LinuxFamilyMaxProcesses-1); status != ChildRegistrationLimit {
		t.Fatalf("zombie did not consume family budget: %d", status)
	}
	if _, ok := ReapChild(root.PID, children[0].PID); !ok {
		t.Fatal("zombie reap failed")
	}
	if _, status := RegisterChild(702, root.PID, LinuxFamilyMaxProcesses-1); status != ChildRegistrationOK {
		t.Fatalf("reaped slot was not reusable: %d", status)
	}
	global, family := ProcessCounts(root.FamilyID)
	if global != LinuxFamilyMaxProcesses || family != LinuxFamilyMaxProcesses {
		t.Fatalf("counts = global %d family %d", global, family)
	}
	if _, status := RegisterChild(703, root.PID, LinuxGlobalMaxProcesses); status != ChildRegistrationLimit {
		t.Fatalf("native reserve status = %d", status)
	}
}

func TestChildRegistrationEnforcesGlobalCompatibilityBudget(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(800, 1, 0)
	for index := 1; index < LinuxGlobalMaxProcesses; index++ {
		if _, ok := installProcessRecord(process.ID(800+index), 1, 0); !ok {
			t.Fatalf("root %d registration failed", index)
		}
	}
	if _, status := RegisterChild(900, root.PID, 1); status != ChildRegistrationLimit {
		t.Fatalf("global overflow status = %d", status)
	}
	if global, _ := ProcessCounts(0); global != LinuxGlobalMaxProcesses {
		t.Fatalf("global count = %d", global)
	}
}

func TestExitProcessReleasesHeavyState(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x9000, 400, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x9000)
	pid := state.PID
	familyID := state.FamilyID
	released := make([]FileDescription, 0, 3)
	if _, found := VFSContext(familyID); !found {
		t.Fatal("VFS context missing")
	}
	if !ExitProcess(0x9000, Termination{Status: 23}, func(description FileDescription) {
		released = append(released, description)
	}) {
		t.Fatal("exit failed")
	}
	if len(released) != 3 || released[0].Kind != FDStreamReader || released[1].Kind != FDStreamWriter || released[2].Kind != FDStreamWriter {
		t.Fatalf("released descriptions = %#v", released)
	}
	if _, found := FindProcess(0x9000); found {
		t.Fatal("exit retained heavy compatibility state")
	}
	if _, found := FindProcessRecord(pid); found {
		t.Fatal("PID 1 child remained as a zombie")
	}
	if _, found := VFSContext(familyID); found {
		t.Fatal("final process retained its VFS context")
	}
	if ExitProcess(0x9000, Termination{Status: 23}, nil) {
		t.Fatal("duplicate exit succeeded")
	}
}

func TestForkProcessSharesDescriptionsAndCopiesState(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/persist/work", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xa000, 500, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	parent, _ := FindProcess(0xa000)
	parent.BrkCurrent = 0x602000
	parent.ClearChildTID = 0x1234
	if _, errno := parent.Signals.Mask(abi.SignalBlock, 1<<14, true); errno != 0 {
		t.Fatalf("mask errno = %d", errno)
	}
	file, errno := AllocateFD(0xa000, FileDescription{Kind: FDVFSFile, Object: 77}, true)
	if errno != 0 {
		t.Fatalf("allocate errno = %d", errno)
	}
	record, registered := registerChildForTest(501, parent.PID)
	if !registered || ForkProcess(0xa000, 0xb000, record, 0) || ForkProcess(0xa000, 0xa000, record, 9) {
		t.Fatal("fork accepted an invalid child")
	}
	if !ForkProcess(0xa000, 0xb000, record, 9) || ForkProcess(0xa000, 0xb000, record, 9) {
		t.Fatal("fork did not install exactly one child")
	}
	child, _ := FindProcess(0xb000)
	if child.PID != record.PID || child.PID == parent.PID || child.FamilyID != parent.FamilyID || child.VFSHandle != 9 ||
		!child.Forked || parent.Forked || child.BrkCurrent != 0x602000 || child.ClearChildTID != 0 || string(child.CWD()) != "/persist/work" {
		t.Fatalf("child = %#v cwd=%q", child, child.CWD())
	}
	if mask, _ := child.Signals.Mask(abi.SignalBlock, 0, false); mask != 1<<14 {
		t.Fatalf("child mask = %#x", mask)
	}
	if closeOnExec, open := child.FDs.CloseOnExec(file); !open || !closeOnExec {
		t.Fatal("child lost the descriptor or its close-on-exec bit")
	}
	shared, _ := DescriptionOf(0xa000, uint64(file))
	shared.Offset = 42
	if description, _ := LookupFD(0xb000, file); description.Offset != 42 || description.References != 2 {
		t.Fatalf("child description = %#v", description)
	}
	if context, _ := VFSContext(parent.FamilyID); context.References != 2 {
		t.Fatalf("VFS context references = %d", context.References)
	}
	if _, last, _ := CloseFD(0xb000, file); last {
		t.Fatal("child close released the parent's description")
	}
	if _, found := LookupFD(0xa000, file); !found {
		t.Fatal("child close removed the parent's descriptor")
	}
	var roots [4]uint64
	if count := FamilyRoots(parent.FamilyID, roots[:]); count != 2 || roots[0]+roots[1] != 0xa000+0xb000 {
		t.Fatalf("family roots = %d %#x", count, roots)
	}
	child.HoldVforkParent(7)
	if child.ReleaseVforkParent() != 7 || child.ReleaseVforkParent() != 0 {
		t.Fatal("the vfork parent was not released exactly once")
	}
	familyID := parent.FamilyID
	if !RemoveProcess(0xb000, nil) {
		t.Fatal("child rollback failed")
	}
	if _, found := FindProcessRecord(record.PID); found {
		t.Fatal("rolled-back child kept its PID")
	}
	if context, found := VFSContext(familyID); !found || context.References != 1 {
		t.Fatal("child rollback did not return its VFS context reference")
	}
}

func resetCompatibilityProcesses() {
	processStates = [process.MaxProcesses]ProcessState{}
	processRecords = [process.MaxProcesses]ProcessRecord{}
	openDescriptions = [MaxOpenDescriptions]descriptionSlot{}
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	nextPID = firstPID
	nextFamilyID = 1
}

func registerChildForTest(goosProcessID process.ID, parentPID int32) (ProcessRecord, bool) {
	record, status := RegisterChild(goosProcessID, parentPID, 1)
	return record, status == ChildRegistrationOK
}
