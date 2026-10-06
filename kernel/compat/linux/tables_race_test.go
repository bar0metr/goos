//go:build !goos_validation

package linux

import (
	"sync"
	"testing"
	"time"

	"goos/kernel/handle"
	"goos/kernel/process"
)

// These tests run the personality's tables the way unrelated Linux
// processes share them (ADR 0187): each call goes through the tables' own
// lock, and nothing else orders two families. A barrier starts the racing
// goroutines together, and a deadline turns a deadlock into a failure.

func finishRace(t *testing.T, group *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("racing goroutines did not finish")
	}
}

// fileChurn opens, reads back, and closes descriptors of root, each naming
// its own object, and reports a descriptor that names another's object.
func fileChurn(root uint64, base uint64, rounds int, failures chan<- string) {
	for round := 0; round < rounds; round++ {
		object := base + uint64(round)
		fd, errno := AllocateFD(root, FileDescription{Kind: FDVFSFile, Object: object}, round%2 == 0)
		if errno != 0 {
			fail(failures, "allocate failed")
			return
		}
		if description, found := LookupFD(root, fd); !found || description.Object != object {
			fail(failures, "a descriptor named another call's file")
		}
		if description, last, errno := CloseFD(root, fd); errno != 0 || !last || description.Object != object {
			fail(failures, "close returned another call's file")
		}
	}
}

// fail records a failure without waiting: a full channel already holds
// enough to report.
func fail(failures chan<- string, message string) {
	select {
	case failures <- message:
	default:
	}
}

func installRaceProcess(t *testing.T, root uint64, id process.ID) *ProcessState {
	t.Helper()
	data := linuxStartup(t, "/persist/work", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(root, id, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(root)
	return state
}

func reportFailures(t *testing.T, failures chan string) {
	t.Helper()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

// Two unrelated processes, and two threads of each, open and close files
// at once. Every descriptor names its own call's file, and the description
// table ends where it began.
func TestFileIOOfTwoProcessesAndTwoThreads(t *testing.T) {
	resetCompatibilityProcesses()
	first := installRaceProcess(t, 0x1000, 70)
	second := installRaceProcess(t, 0x2000, 71)
	if first.FamilyID == second.FamilyID {
		t.Fatal("unrelated processes share a family")
	}
	descriptions, references := OpenDescriptionCount()
	start := make(chan struct{})
	failures := make(chan string, 16)
	var group sync.WaitGroup
	for index, root := range []uint64{0x1000, 0x1000, 0x2000, 0x2000} {
		group.Add(1)
		go func(root uint64, base uint64) {
			defer group.Done()
			<-start
			fileChurn(root, base, 2000, failures)
		}(root, uint64(index+1)<<32)
	}
	close(start)
	finishRace(t, &group)
	reportFailures(t, failures)
	if nowDescriptions, nowReferences := OpenDescriptionCount(); nowDescriptions != descriptions || nowReferences != references {
		t.Fatalf("descriptions %d/%d, want %d/%d", nowDescriptions, nowReferences, descriptions, references)
	}
}

// One process forks and drops children while a second execs and a third
// opens and closes files; the three are unrelated, so nothing orders them
// but the tables' lock. A fork and an exec of one family never overlap: the
// family lock orders them. The fork shares the parent's descriptions, the
// exec closes only close-on-exec ones, and the tables end where they began.
func TestForkAndExecWithConcurrentFileIO(t *testing.T) {
	resetCompatibilityProcesses()
	parent := installRaceProcess(t, 0x3000, 80)
	installRaceProcess(t, 0x4000, 81)
	execing := installRaceProcess(t, 0x5000, 82)
	kept, _ := AllocateFD(0x3000, FileDescription{Kind: FDVFSFile, Object: 0x5151}, false)
	survivor, _ := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: 0x5252}, false)
	descriptions, references := OpenDescriptionCount()
	start := make(chan struct{})
	failures := make(chan string, 16)
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		<-start
		fileChurn(0x4000, 1<<40, 2000, failures)
	}()
	go func() {
		defer group.Done()
		<-start
		var released Released
		for round := 0; round < 300; round++ {
			record, registered := registerChildForTest(process.ID(90+round%8), parent.PID)
			if !registered || !ForkProcess(0x3000, 0x8000, record, 9) {
				fail(failures, "fork failed")
				return
			}
			if description, found := LookupFD(0x8000, kept); !found || description.Object != 0x5151 {
				fail(failures, "the child lost the parent's descriptor")
			}
			released.Reset()
			if !RemoveProcess(0x8000, &released) {
				fail(failures, "the child was not removed")
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		<-start
		var released Released
		for round := 0; round < 300; round++ {
			if _, errno := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: 0x6000 + uint64(round)}, true); errno != 0 {
				fail(failures, "allocate before exec failed")
				return
			}
			released.Reset()
			execing.CloseOnExec(&released)
			if released.Len() != 1 || released.At(0).Object != 0x6000+uint64(round) {
				fail(failures, "exec closed the wrong descriptors")
			}
		}
	}()
	close(start)
	finishRace(t, &group)
	reportFailures(t, failures)
	if description, found := LookupFD(0x3000, kept); !found || description.Object != 0x5151 {
		t.Fatal("the parent lost a descriptor")
	}
	if description, found := LookupFD(0x5000, survivor); !found || description.Object != 0x5252 {
		t.Fatal("exec closed a descriptor without close-on-exec")
	}
	if nowDescriptions, nowReferences := OpenDescriptionCount(); nowDescriptions != descriptions || nowReferences != references {
		t.Fatalf("descriptions %d/%d, want %d/%d", nowDescriptions, nowReferences, descriptions, references)
	}
}
