package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

type repairList []RepairAction

func (repairs *repairList) Repair(action RepairAction) { *repairs = append(*repairs, action) }

func hasRepair(repairs repairList, code RepairCode) bool {
	for _, repair := range repairs {
		if repair.Code == code {
			return true
		}
	}
	return false
}

func repaired(t *testing.T, device Device) (RepairReport, repairList) {
	t.Helper()
	var actions repairList
	report := Repair(device, nil, &actions)
	if report.Status != ramfs.StatusOK || report.After.Issues != 0 {
		t.Fatalf("repair=%+v actions=%+v", report, actions)
	}
	return report, actions
}

func TestRepairIsIdempotentOnCleanVolume(t *testing.T) {
	device, _ := checkedVolume(t)
	first, actions := repaired(t, device)
	if first.Repaired != 0 || len(actions) != 0 {
		t.Fatalf("clean repair=%+v actions=%+v", first, actions)
	}
	second, actions := repaired(t, device)
	if second.Repaired != 0 || len(actions) != 0 {
		t.Fatalf("second repair=%+v actions=%+v", second, actions)
	}
}

func TestRepairSynchronizesFATAndFSInfo(t *testing.T) {
	device, volume := checkedVolume(t)
	fs := &volume.fs
	put32(device.data[fs.fatStart+fs.fatSize+8:], 17)
	put32(device.data[fs.fsInfoPrimary+fsInfoFreeCount:], 1)
	put32(device.data[fs.fsInfoBackup+fsInfoFreeCount:], 2)
	_, actions := repaired(t, device)
	if !hasRepair(actions, RepairFATCopies) || !hasRepair(actions, RepairFSInfo) {
		t.Fatalf("actions=%+v", actions)
	}
	if mismatch, status := fs.CompareFATs(); status != ramfs.StatusOK || mismatch.Entries != 0 {
		t.Fatalf("mismatch=%+v status=%v", mismatch, status)
	}
}

func TestRepairRestoresBootMetadataInEitherDirection(t *testing.T) {
	for _, primary := range []bool{true, false} {
		name := "backup"
		if primary {
			name = "primary"
		}
		t.Run(name, func(t *testing.T) {
			device, volume := checkedVolume(t)
			sectorSize := volume.fs.sectorSize
			offset := uint64(0)
			want := RepairPrimaryBoot
			if !primary {
				offset = backupCandidate * sectorSize
				want = RepairBackupBoot
			}
			device.data[offset+bpbSignature] = 0
			_, actions := repaired(t, device)
			if !hasRepair(actions, want) {
				t.Fatalf("actions=%+v", actions)
			}
			var fs FileSystem
			if _, ok := fs.Mount(device); !ok || fs.Health().Degraded() {
				t.Fatalf("health=%+v refusal=%v", fs.Health(), fs.Refused())
			}
		})
	}
}

func TestRepairRemovesBrokenLFNAndReclaimsLostCluster(t *testing.T) {
	device, volume := checkedVolume(t)
	root, _ := volume.Root()
	id, status := volume.CreateName(root, mustName(t, "Long mixed name.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	fs := &volume.fs
	dir, index, _, _ := fs.node(id)
	long, _ := fs.readEntry(&dir, index-1)
	long[13]++
	if fs.writeEntry(&dir, index-1, long) != ramfs.StatusOK {
		t.Fatal("lfn corruption")
	}
	lost := uint32(20)
	if fs.setFAT(lost, endOfChainMark) != ramfs.StatusOK || !fs.Flush() {
		t.Fatal("lost cluster corruption")
	}
	_, actions := repaired(t, device)
	if !hasRepair(actions, RepairLFN) || !hasRepair(actions, RepairLostCluster) {
		t.Fatalf("actions=%+v", actions)
	}
	var checked FileSystem
	if _, ok := checked.Mount(device); !ok {
		t.Fatal("remount")
	}
	if value, _ := checked.fat(lost); value != 0 {
		t.Fatalf("lost cluster value=%#x", value)
	}
}

func TestRepairTrimsChainBeyondEOF(t *testing.T) {
	device, volume := checkedVolume(t)
	root, _ := volume.Root()
	id, status := volume.CreateName(root, mustName(t, "file"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status = volume.Write(id, 0, []byte("data")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	fs := &volume.fs
	_, _, entry, _ := fs.node(id)
	first, extra := entryCluster(entry), uint32(30)
	if fs.setFAT(first, extra) != ramfs.StatusOK || fs.setFAT(extra, endOfChainMark) != ramfs.StatusOK || !fs.Flush() {
		t.Fatal("chain corruption")
	}
	_, actions := repaired(t, device)
	if !hasRepair(actions, RepairChain) || !hasRepair(actions, RepairLostCluster) {
		t.Fatalf("actions=%+v", actions)
	}
	var checked FileSystem
	if _, ok := checked.Mount(device); !ok {
		t.Fatal("remount")
	}
	if value, _ := checked.fat(first); value < endOfChain {
		t.Fatalf("first=%#x", value)
	}
	if value, _ := checked.fat(extra); value != 0 {
		t.Fatalf("extra=%#x", value)
	}
}

func TestRepairLeavesCrossLinkedFilesUnresolved(t *testing.T) {
	device, volume := checkedVolume(t)
	root, _ := volume.Root()
	first := writeFile(t, &volume.fs, root, "first", "first")
	second := writeFile(t, &volume.fs, root, "second", "second")
	fs := &volume.fs
	_, _, firstEntry, _ := fs.node(first)
	dir, index, secondEntry, _ := fs.node(second)
	setEntryCluster(&secondEntry, entryCluster(firstEntry))
	if fs.writeEntry(&dir, index, secondEntry) != ramfs.StatusOK || !fs.Flush() {
		t.Fatal("cross-link corruption")
	}
	var actions repairList
	report := Repair(device, nil, &actions)
	if report.After.Issues == 0 || !hasIssueAfter(device, CheckCrossLinked) {
		t.Fatalf("report=%+v actions=%+v", report, actions)
	}
}

func hasIssueAfter(device Device, code CheckCode) bool {
	var issues issueList
	Check(device, &issues)
	return hasIssue(issues, code)
}
