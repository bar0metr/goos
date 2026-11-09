package volume

import (
	"testing"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsdetect"
	"goos/userspace/partition"
)

func TestParseSource(t *testing.T) {
	guid, _ := partition.ParseGUID("0FC63DAF-8483-4772-8E79-3D69D8477DE4")
	for text, want := range map[string]partition.Selector{
		"part:2":  {Kind: partition.SelectIndex, Index: 2},
		"part:12": {Kind: partition.SelectIndex, Index: 12},
		"guid:0FC63DAF-8483-4772-8E79-3D69D8477DE4": {Kind: partition.SelectGUID, GUID: guid},
	} {
		got, ok := ParseSource([]byte(text))
		if !ok || got != want {
			t.Errorf("%s: %+v %v", text, got, ok)
		}
	}
	named, ok := ParseSource([]byte("name:goos-data"))
	if !ok || named.Kind != partition.SelectName || string(named.Name[:named.NameSize]) != "goos-data" {
		t.Errorf("name: %+v %v", named, ok)
	}
	for _, text := range []string{"", "part:", "part:0", "part:x", "guid:zz", "guid:00000000-0000-0000-0000-000000000000", "whole", "disk:1", "name:"} {
		if _, ok := ParseSource([]byte(text)); ok {
			t.Errorf("accepted %q", text)
		}
	}
}

func request(t *testing.T, kind, source, path string) Request {
	t.Helper()
	request, status := MakeRequest([]byte(kind), []byte(source), []byte(path))
	if status != OK {
		t.Fatalf("%s %s %s: %v", kind, source, path, status)
	}
	return request
}

func TestMakeRequest(t *testing.T) {
	if r := request(t, "", "part:2", "/mnt/a"); r.Type != 0 || string(r.PathBytes()) != "/mnt/a" {
		t.Fatalf("%+v", r)
	}
	if r := request(t, "ext4", "part:2", "/mnt/a"); r.Type != ramfs.FilesystemExt4 {
		t.Fatalf("%+v", r)
	}
	for _, tc := range []struct {
		kind, source, path string
		want               Status
	}{
		{"xfs", "part:2", "/mnt/a", InvalidType},
		{"auto", "sda1", "/mnt/a", InvalidSource},
		{"auto", "part:2", "/system", InvalidPath},
		{"auto", "part:2", "/mnt/a/b", InvalidPath},
	} {
		if _, status := MakeRequest([]byte(tc.kind), []byte(tc.source), []byte(tc.path)); status != tc.want {
			t.Errorf("%+v: %v", tc, status)
		}
	}
}

func ext4Probe(identity byte) fsdetect.Result {
	result := fsdetect.Result{Status: fsdetect.Recognized, Type: ramfs.FilesystemExt4, IdentitySize: 16}
	result.Identity[0] = identity
	return result
}

func TestMountLifecycle(t *testing.T) {
	var table Table
	slot, status := table.Reserve(request(t, "auto", "part:2", "/mnt/a"))
	if status != OK || slot != 1 {
		t.Fatalf("reserve: %d %v", slot, status)
	}
	if status := table.Probed(slot, ext4Probe(1)); status != OK || table.Slot(slot).Driver.Name != "ext4" {
		t.Fatalf("probe: %v", status)
	}
	generation := table.Launching(slot)
	if table.Attached(slot, generation, vfs.StatusOK) != OK || table.Slot(slot).State != Mounted {
		t.Fatal("attach")
	}
	// A stale notice changes nothing.
	if table.Lost(slot, generation+5) {
		t.Fatal("stale notice restarted the volume")
	}
	for restart := 0; restart < MaxRestarts; restart++ {
		if !table.Lost(slot, generation) {
			t.Fatalf("restart %d refused", restart)
		}
		if status := table.Probed(slot, ext4Probe(1)); status != OK {
			t.Fatalf("reprobe: %v", status)
		}
		generation = table.Launching(slot)
		if table.Attached(slot, generation, vfs.StatusOK) != OK {
			t.Fatal("reattach")
		}
	}
	if table.Lost(slot, generation) || table.Slot(slot).State != Offline {
		t.Fatal("restart budget not enforced")
	}
	table.Release(slot)
	if table.Slot(slot).State != Free {
		t.Fatal("release")
	}
}

func TestReplacementMustFindTheSameVolume(t *testing.T) {
	var table Table
	slot, _ := table.Reserve(request(t, "auto", "name:data", "/mnt/a"))
	table.Probed(slot, ext4Probe(1))
	generation := table.Launching(slot)
	table.Attached(slot, generation, vfs.StatusOK)
	table.Lost(slot, generation)
	if status := table.Probed(slot, ext4Probe(2)); status != TypeMismatch || table.Slot(slot).State != Offline {
		t.Fatalf("another volume accepted: %v", status)
	}
	fat := fsdetect.Result{Status: fsdetect.Recognized, Type: ramfs.FilesystemFAT32, IdentitySize: 4}
	if status := table.Probed(slot, fat); status != TypeMismatch {
		t.Fatalf("another type accepted: %v", status)
	}
}

func TestReserveConflicts(t *testing.T) {
	var table Table
	table.Reserve(request(t, "auto", "part:2", "/mnt/a"))
	if _, status := table.Reserve(request(t, "auto", "part:3", "/mnt/a")); status != PathInUse {
		t.Fatalf("same path: %v", status)
	}
	if _, status := table.Reserve(request(t, "auto", "part:2", "/mnt/b")); status != PartitionInUse {
		t.Fatalf("same partition: %v", status)
	}
	for index := 0; index < Slots-1; index++ {
		if _, status := table.Reserve(request(t, "auto", "part:"+string(rune('3'+index)), "/mnt/"+string(rune('b'+index)))); status != OK {
			t.Fatalf("slot %d: %v", index+2, status)
		}
	}
	if _, status := table.Reserve(request(t, "auto", "part:9", "/mnt/z")); status != NoSlot {
		t.Fatalf("full table: %v", status)
	}
	if slot, found := table.Find([]byte("/mnt/b")); !found || slot != 2 {
		t.Fatalf("find: %d %v", slot, found)
	}
}

func TestProbeRefusals(t *testing.T) {
	var table Table
	slot, _ := table.Reserve(request(t, "fat32", "part:2", "/mnt/a"))
	if status := table.Probed(slot, ext4Probe(1)); status != TypeMismatch {
		t.Fatalf("type forced: %v", status)
	}
	if status := table.Probed(slot, fsdetect.Result{Status: fsdetect.Ambiguous}); status != Ambiguous {
		t.Fatalf("ambiguous: %v", status)
	}
	if status := table.Probed(slot, fsdetect.Result{Status: fsdetect.Unknown}); status != Unrecognized {
		t.Fatalf("unknown: %v", status)
	}
	generation := table.Launching(slot)
	if table.Attached(slot, generation, vfs.StatusCorrupt) != Refused || table.Slot(slot).Detail != vfs.StatusCorrupt {
		t.Fatal("refused attach")
	}
	if table.Lost(slot, generation) {
		t.Fatal("an unmounted volume was restarted")
	}
}

func TestStatusText(t *testing.T) {
	for status := OK; status <= lastStatus; status++ {
		if status.String() == "" || status.String() == "unknown status" {
			t.Errorf("status %d has no text", status)
		}
	}
}
