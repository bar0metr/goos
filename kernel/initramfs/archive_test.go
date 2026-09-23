package initramfs

import (
	"archive/tar"
	"bytes"
	"goos/kernel/ramfs"
	"testing"
	"time"
)

func testArchiveBytes(t testing.TB) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)
	epoch := time.Unix(0, 0)
	headers := []*tar.Header{
		{Name: "system/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/motd", Mode: 0o444, Size: 5, Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/coreutils", Mode: 0o555, Size: 4, Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/ls", Mode: 0o555, Typeflag: tar.TypeLink, Linkname: "system/bin/coreutils", ModTime: epoch, Format: tar.FormatUSTAR},
	}
	payloads := []string{"", "GoOS\n", "", "CODE", ""}
	for index, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("header %s: %v", header.Name, err)
		}
		if payloads[index] != "" {
			if _, err := writer.Write([]byte(payloads[index])); err != nil {
				t.Fatalf("payload %s: %v", header.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buffer.Bytes()
}

func testArchive(t testing.TB) *Archive {
	t.Helper()
	archive := &Archive{}
	if !archive.Init(testArchiveBytes(t)) {
		t.Fatal("archive init failed")
	}
	return archive
}

func open(t testing.TB, archive *Archive, parent ramfs.NodeID, component string) ramfs.NodeID {
	t.Helper()
	name, valid := ramfs.MakeName([]byte(component))
	if !valid {
		t.Fatalf("invalid component %q", component)
	}
	node, status := archive.OpenName(parent, name)
	if status != ramfs.StatusOK {
		t.Fatalf("open %q: %d", component, status)
	}
	return node
}

func TestArchiveResolvesDirectoriesFilesAndLinks(t *testing.T) {
	archive := testArchive(t)
	system := open(t, archive, archive.Root(), "system")
	if stat, status := archive.Stat(system); status != ramfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat system: %d %d", status, stat.Kind)
	}
	bin := open(t, archive, system, "bin")
	command := open(t, archive, bin, "ls")

	// A hard link reports and reads the program it names.
	stat, status := archive.Stat(command)
	if status != ramfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size != 4 {
		t.Fatalf("stat ls: %d %+v", status, stat)
	}
	data := make([]byte, 8)
	count, status := archive.Read(command, 0, data)
	if status != ramfs.StatusOK || string(data[:count]) != "CODE" {
		t.Fatalf("read ls: %d %q", status, data[:count])
	}
	if count, status := archive.Read(command, 2, data); status != ramfs.StatusOK || string(data[:count]) != "DE" {
		t.Fatalf("read ls at offset: %d %q", status, data[:count])
	}
	if _, status := archive.Read(command, 9, data); status != ramfs.StatusInvalidArgument {
		t.Fatalf("read past end: %d", status)
	}
	if _, status := archive.Read(bin, 0, data); status != ramfs.StatusIsDirectory {
		t.Fatalf("read directory: %d", status)
	}
	motd := open(t, archive, system, "motd")
	if count, status := archive.Read(motd, 0, data); status != ramfs.StatusOK || string(data[:count]) != "GoOS\n" {
		t.Fatalf("read motd: %d %q", status, data[:count])
	}
}

func TestArchiveListsOnlyDirectChildren(t *testing.T) {
	archive := testArchive(t)
	list := func(parent ramfs.NodeID) []string {
		t.Helper()
		var names []string
		for cursor := 0; ; {
			entry, next, status := archive.ReadDir(parent, cursor)
			if status == ramfs.StatusNotFound {
				return names
			}
			if status != ramfs.StatusOK || next <= cursor {
				t.Fatalf("readdir: %d cursor %d -> %d", status, cursor, next)
			}
			names = append(names, string(entry.Name[:entry.Size]))
			cursor = next
		}
	}
	if names := list(archive.Root()); len(names) != 1 || names[0] != "system" {
		t.Fatalf("root listing = %v", names)
	}
	system := open(t, archive, archive.Root(), "system")
	names := list(system)
	if len(names) != 2 || names[0] != "motd" || names[1] != "bin" {
		t.Fatalf("/system listing = %v", names)
	}
	bin := open(t, archive, system, "bin")
	if names := list(bin); len(names) != 2 || names[0] != "coreutils" || names[1] != "ls" {
		t.Fatalf("/system/bin listing = %v", names)
	}
	if _, _, status := archive.ReadDir(bin, 1); status != ramfs.StatusInvalidArgument {
		t.Fatalf("misaligned cursor: %d", status)
	}
	motd := open(t, archive, system, "motd")
	if _, _, status := archive.ReadDir(motd, 0); status != ramfs.StatusNotDirectory {
		t.Fatalf("readdir of a file: %d", status)
	}
}

func TestArchiveRefusesEveryMutation(t *testing.T) {
	archive := testArchive(t)
	name, _ := ramfs.MakeName([]byte("new"))
	root := archive.Root()
	if _, status := archive.CreateName(root, name); status != ramfs.StatusPermissionDenied {
		t.Fatalf("create: %d", status)
	}
	if _, status := archive.MkdirName(root, name); status != ramfs.StatusPermissionDenied {
		t.Fatalf("mkdir: %d", status)
	}
	system := open(t, archive, root, "system")
	motd := open(t, archive, system, "motd")
	if _, status := archive.Write(motd, 0, []byte("x")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("write: %d", status)
	}
	if status := archive.Truncate(motd, 0); status != ramfs.StatusPermissionDenied {
		t.Fatalf("truncate: %d", status)
	}
	if status := archive.RenameName(motd, system, name); status != ramfs.StatusPermissionDenied {
		t.Fatalf("rename: %d", status)
	}
	if status := archive.UnlinkName(system, name); status != ramfs.StatusPermissionDenied {
		t.Fatalf("unlink: %d", status)
	}
}

func TestArchiveRejectsMalformedImages(t *testing.T) {
	if (&Archive{}).Init(nil) {
		t.Fatal("empty archive accepted")
	}
	if (&Archive{}).Init(make([]byte, 64)) {
		t.Fatal("short archive accepted")
	}
	if (&Archive{}).Init(make([]byte, 2*blockSize)) {
		t.Fatal("archive with no members accepted")
	}
	corrupt := testArchiveBytes(t)
	corrupt[0] = 'x'
	if (&Archive{}).Init(corrupt) {
		t.Fatal("bad checksum accepted")
	}
	// One of the two trailing zero blocks still terminates the archive.
	trimmed := testArchiveBytes(t)
	if !(&Archive{}).Init(trimmed[:len(trimmed)-blockSize]) {
		t.Fatal("archive with one terminator rejected")
	}
	// An image cut short must not expose the prefix that happened to arrive.
	short := testArchiveBytes(t)
	if (&Archive{}).Init(short[:3*blockSize+1]) {
		t.Fatal("truncated archive accepted")
	}
	if (&Archive{}).Init(short[:4*blockSize]) {
		t.Fatal("archive without a terminator accepted")
	}
}

func TestArchiveNodesAreStableAndBounded(t *testing.T) {
	archive := testArchive(t)
	system := open(t, archive, archive.Root(), "system")
	if again := open(t, archive, archive.Root(), "system"); again != system {
		t.Fatalf("node id changed: %d != %d", again, system)
	}
	if system == archive.Root() {
		t.Fatal("member reused the root node id")
	}
	if _, status := archive.Stat(ramfs.NodeID(1 << 20)); status != ramfs.StatusNotFound {
		t.Fatalf("out-of-range node accepted: %d", status)
	}
	name, _ := ramfs.MakeName([]byte("missing"))
	if _, status := archive.OpenName(system, name); status != ramfs.StatusNotFound {
		t.Fatalf("missing name: %d", status)
	}
	if _, status := archive.OpenName(ramfs.NodeID(1<<20), name); status != ramfs.StatusNotFound {
		t.Fatalf("lookup in a bad parent: %d", status)
	}
}
