package linux

import "testing"

// Tables counts what each shared table holds: two families of three and one
// process, an eventfd and a directory, and they go when released.
func TestTablesCountsSharedTableUse(t *testing.T) {
	resetCompatibilityProcesses()
	defer resetCompatibilityProcesses()
	before := Tables()
	if before.Processes != 0 || before.BusiestFamily != 0 || before.Terminals != 0 || before.Descriptions != 0 {
		t.Fatalf("empty tables %+v", before)
	}
	root, _ := installProcessRecord(900, 1, 0)
	if _, ok := registerChildForTest(901, root.PID); !ok {
		t.Fatal("first child")
	}
	if _, ok := registerChildForTest(902, root.PID); !ok {
		t.Fatal("second child")
	}
	installProcessRecord(903, 1, 0)
	event, errno := CreateEventCounter(root.FamilyID, 0, false)
	if errno != 0 {
		t.Fatalf("eventfd %d", errno)
	}
	directory, ok := RetainDirectory(root.FamilyID, []byte("/tmp"))
	if !ok {
		t.Fatal("directory")
	}
	use := Tables()
	if use.Processes != 4 || use.BusiestFamily != 3 || use.EventCounters != before.EventCounters+1 ||
		use.Directories != before.Directories+1 {
		t.Fatalf("tables %+v, before %+v", use, before)
	}
	ReleaseEventCounter(root.FamilyID, event)
	ReleaseDirectory(root.FamilyID, directory)
	if after := Tables(); after.EventCounters != before.EventCounters || after.Directories != before.Directories {
		t.Fatalf("after release %+v", after)
	}
}
