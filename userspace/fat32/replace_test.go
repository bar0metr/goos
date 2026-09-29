package fat32

import (
	"bytes"
	"goos/kernel/ramfs"
	"testing"
)

type issueCodes []CheckCode

func (codes *issueCodes) Issue(issue CheckIssue) { *codes = append(*codes, issue.Code) }

// replaceFixture writes old contents under "doc.txt" and new contents under a
// temporary name beside it, as an editor's save does before it replaces.
func replaceFixture(t *testing.T, device *memoryDevice) (*FileSystem, ramfs.NodeID, ramfs.NodeID, ramfs.Name, []byte, []byte) {
	t.Helper()
	fs := &FileSystem{}
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatal("mount failed")
	}
	old := bytes.Repeat([]byte("old line\n"), 170)
	updated := bytes.Repeat([]byte("new contents\n"), 160)
	name, _ := ramfs.MakeName([]byte("doc.txt"))
	temp, _ := ramfs.MakeName([]byte(".doc.txt.~save"))
	target, status := fs.CreateName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if count, status := fs.Write(target, 0, old); status != ramfs.StatusOK || count != len(old) {
		t.Fatalf("write old %d %d", count, status)
	}
	source, status := fs.CreateName(root, temp)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if count, status := fs.Write(source, 0, updated); status != ramfs.StatusOK || count != len(updated) {
		t.Fatalf("write new %d %d", count, status)
	}
	return fs, root, source, name, old, updated
}

func readNamed(t *testing.T, fs *FileSystem, root ramfs.NodeID, text string) ([]byte, ramfs.Status) {
	t.Helper()
	name, _ := ramfs.MakeName([]byte(text))
	id, status := fs.OpenName(root, name)
	if status != ramfs.StatusOK {
		return nil, status
	}
	stat, status := fs.Stat(id)
	if status != ramfs.StatusOK {
		return nil, status
	}
	data := make([]byte, stat.Size)
	count, status := fs.Read(id, 0, data)
	return data[:count], status
}

func TestReplaceNameSwapsContentsAndRemovesSource(t *testing.T) {
	device := formattedDevice()
	fs, root, source, name, _, updated := replaceFixture(t, device)
	info, _ := fs.FilesystemInfo()
	if status := fs.ReplaceName(source, root, name); status != ramfs.StatusOK {
		t.Fatalf("replace = %d", status)
	}
	if data, status := readNamed(t, fs, root, "doc.txt"); status != ramfs.StatusOK || !bytes.Equal(data, updated) {
		t.Fatalf("target after replace: %d %q", status, data)
	}
	if _, status := readNamed(t, fs, root, ".doc.txt.~save"); status != ramfs.StatusNotFound {
		t.Fatalf("source still named: %d", status)
	}
	after, _ := fs.FilesystemInfo()
	if after.FreeBytes <= info.FreeBytes {
		t.Fatalf("old chain not freed: %d -> %d", info.FreeBytes, after.FreeBytes)
	}
	// The synthetic volume has no FSInfo sector, which Check always reports.
	var codes issueCodes
	Check(device, &codes)
	for _, code := range codes {
		if code != CheckFSInfo {
			t.Fatalf("check after replace: %v", codes)
		}
	}
}

func TestReplaceNameWithoutTargetRenames(t *testing.T) {
	fs, root, source, _, _, updated := replaceFixture(t, formattedDevice())
	fresh, _ := ramfs.MakeName([]byte("fresh.txt"))
	if status := fs.ReplaceName(source, root, fresh); status != ramfs.StatusOK {
		t.Fatalf("replace onto a new name = %d", status)
	}
	if data, status := readNamed(t, fs, root, "fresh.txt"); status != ramfs.StatusOK || !bytes.Equal(data, updated) {
		t.Fatalf("renamed file: %d", status)
	}
}

func TestReplaceNameRefusals(t *testing.T) {
	fs, root, source, name, old, _ := replaceFixture(t, formattedDevice())
	directory, _ := ramfs.MakeName([]byte("dir"))
	if _, status := fs.MkdirName(root, directory); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.ReplaceName(source, root, directory); status != ramfs.StatusIsDirectory {
		t.Fatalf("replace a directory = %d", status)
	}
	target, _ := fs.OpenName(root, name)
	object, status := fs.AcquireObject(target)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.ReplaceName(source, root, name); status != ramfs.StatusPermissionDenied {
		t.Fatalf("replace an open file = %d", status)
	}
	fs.ReleaseObject(object)
	if status := fs.ReplaceName(target, root, name); status != ramfs.StatusInvalidArgument {
		t.Fatalf("replace a file with itself = %d", status)
	}
	if data, _ := readNamed(t, fs, root, "doc.txt"); !bytes.Equal(data, old) {
		t.Fatal("refused replace changed the target")
	}
}

// TestReplaceNameSurvivesInterruptionAtEveryWrite stops the device at each
// write a replacement makes, as a power loss would, and mounts what is left:
// the target must hold all of its old contents or all of the new ones, and the
// volume must have lost clusters at worst, never a chain two entries share.
func TestReplaceNameSurvivesInterruptionAtEveryWrite(t *testing.T) {
	probe := formattedDevice()
	fs, root, source, name, _, _ := replaceFixture(t, probe)
	before := probe.writes
	if status := fs.ReplaceName(source, root, name); status != ramfs.StatusOK {
		t.Fatalf("replace = %d", status)
	}
	total := probe.writes - before
	if total == 0 {
		t.Fatal("replace wrote nothing")
	}
	for cut := 1; cut <= total; cut++ {
		device := formattedDevice()
		fs, root, source, name, old, updated := replaceFixture(t, device)
		device.failAt = device.writes + cut
		fs.ReplaceName(source, root, name)
		device.failAt = 0
		var mounted FileSystem
		mountedRoot, ok := mounted.Mount(device)
		if !ok {
			t.Fatalf("cut %d: remount failed", cut)
		}
		data, status := readNamed(t, &mounted, mountedRoot, "doc.txt")
		if status != ramfs.StatusOK || !bytes.Equal(data, old) && !bytes.Equal(data, updated) {
			t.Fatalf("cut %d: target holds %d bytes (status %d), neither version", cut, len(data), status)
		}
		var codes issueCodes
		Check(device, &codes)
		for _, code := range codes {
			if code != CheckLostCluster && code != CheckFSInfo && code != CheckLFN {
				t.Fatalf("cut %d: check found %v", cut, codes)
			}
		}
	}
}
