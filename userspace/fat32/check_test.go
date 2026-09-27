package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

type issueList []CheckIssue

func (issues *issueList) Issue(issue CheckIssue) { *issues = append(*issues, issue) }

func hasIssue(issues issueList, code CheckCode) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func checkedVolume(t *testing.T) (*memoryDevice, *Volume) {
	t.Helper()
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	volume := &Volume{}
	if !volume.Mount(device) {
		t.Fatal("mount")
	}
	return device, volume
}

func TestCheckAcceptsCleanVolume(t *testing.T) {
	device, _ := checkedVolume(t)
	var issues issueList
	report := Check(device, &issues)
	if report.Status != ramfs.StatusOK || report.Issues != 0 || len(issues) != 0 || report.Directories != 1 {
		t.Fatalf("report=%+v issues=%+v", report, issues)
	}
}

func TestCheckFindsLostAndCrossLinkedClusters(t *testing.T) {
	device, volume := checkedVolume(t)
	root, _ := volume.Root()
	first, status := volume.CreateName(root, mustName(t, "first"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status = volume.Write(first, 0, []byte("data")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	second, status := volume.CreateName(root, mustName(t, "second"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status = volume.Write(second, 0, []byte("more")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	fs := &volume.fs
	_, _, firstEntry, _ := fs.node(first)
	_, _, secondEntry, _ := fs.node(second)
	firstCluster, lostCluster := entryCluster(firstEntry), entryCluster(secondEntry)
	// Point the second directory entry at the first chain, leaving its old
	// chain allocated and making the live files cross-linked.
	dir, index, entry, _ := fs.node(second)
	setEntryCluster(&entry, firstCluster)
	if fs.writeEntry(&dir, index, entry) != ramfs.StatusOK || !fs.Flush() {
		t.Fatal("corruption setup")
	}
	var issues issueList
	report := Check(device, &issues)
	if report.Issues == 0 || !hasIssue(issues, CheckCrossLinked) || !hasIssue(issues, CheckLostCluster) {
		t.Fatalf("lost=%d report=%+v issues=%+v", lostCluster, report, issues)
	}
}

func TestCheckFindsInvalidLongNameChain(t *testing.T) {
	device, volume := checkedVolume(t)
	root, _ := volume.Root()
	id, status := volume.CreateName(root, mustName(t, "Long mixed name.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	fs := &volume.fs
	dir, index, _, _ := fs.node(id)
	long, status := fs.readEntry(&dir, index-1)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	long[13]++
	if fs.writeEntry(&dir, index-1, long) != ramfs.StatusOK {
		t.Fatal("corruption setup")
	}
	var issues issueList
	Check(device, &issues)
	if !hasIssue(issues, CheckLFN) {
		t.Fatalf("issues=%+v", issues)
	}
}
