package fat32

import (
	"testing"

	"goos/kernel/ramfs"
)

func TestOpenObjectSurvivesRenameAcrossDirectories(t *testing.T) {
	_, volume := checkedVolume(t)
	root, _ := volume.Root()
	left := mkdir(t, &volume.fs, root, "left")
	right := mkdir(t, &volume.fs, root, "right")
	file := writeFile(t, &volume.fs, left, "file", "payload")
	object, status := volume.AcquireObject(file)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status = volume.RenameName(file, right, mustName(t, "moved")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	data := make([]byte, 7)
	if count, status := volume.Read(object, 0, data); status != ramfs.StatusOK || count != len(data) || string(data) != "payload" {
		t.Fatalf("read=%d,%v,%q", count, status, data)
	}
	if _, status = volume.ReleaseObject(object); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status = volume.Read(object, 0, data); status != ramfs.StatusNotFound {
		t.Fatalf("released object status=%v", status)
	}
}

func TestUnlinkOpenObjectIsRefusedUntilClose(t *testing.T) {
	_, volume := checkedVolume(t)
	root, _ := volume.Root()
	file := writeFile(t, &volume.fs, root, "file", "payload")
	object, status := volume.AcquireObject(file)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status = volume.UnlinkName(root, mustName(t, "file")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("unlink open=%v", status)
	}
	second, status := volume.AcquireObject(file)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if location, releaseStatus := volume.ReleaseObject(object); releaseStatus != ramfs.StatusOK || location != object {
		t.Fatalf("release first=%v,%v", location, releaseStatus)
	}
	if status = volume.UnlinkName(root, mustName(t, "file")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("unlink with second object=%v", status)
	}
	if _, status = volume.ReleaseObject(second); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status = volume.UnlinkName(root, mustName(t, "file")); status != ramfs.StatusOK {
		t.Fatalf("unlink closed=%v", status)
	}
}
