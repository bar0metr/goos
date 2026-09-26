package partition_test

import (
	"bytes"
	"fmt"
	"goos/internal/imagefixture"
	"goos/kernel/ramfs"
	blockproto "goos/userspace/block"
	"goos/userspace/fat32"
	"goos/userspace/partition"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Interoperability with the host's partitioning. On macOS, hdiutil makes
// MBR and GPT disk images with a FAT32 volume, gpt(8) and fdisk(8) read
// tables, and the host mounts and checks what this code wrote. Elsewhere the
// host cases are skipped and the images kept in testdata stand in for them.

type fileDevice struct {
	file *os.File
	size uint64
}

func (device fileDevice) ReadAt(offset uint64, target []byte) bool {
	_, err := device.file.ReadAt(target, int64(offset))
	return err == nil && offset+uint64(len(target)) <= device.size
}
func (device fileDevice) WriteAt(offset uint64, data []byte) bool {
	if offset+uint64(len(data)) > device.size {
		return false
	}
	_, err := device.file.WriteAt(data, int64(offset))
	return err == nil
}
func (device fileDevice) Flush() bool      { return device.file.Sync() == nil }
func (device fileDevice) Capacity() uint64 { return device.size }

// sliceDevice is a volume as the block service lets a filesystem reach it.
type sliceDevice struct {
	device fileDevice
	slice  blockproto.Slice
}

func (device sliceDevice) ReadAt(offset uint64, target []byte) bool {
	at, ok := device.slice.Translate(offset, uint64(len(target)))
	return ok && device.device.ReadAt(at, target)
}
func (device sliceDevice) WriteAt(offset uint64, data []byte) bool {
	at, ok := device.slice.Translate(offset, uint64(len(data)))
	return ok && device.slice.Writable && device.device.WriteAt(at, data)
}
func (device sliceDevice) Flush() bool      { return device.device.Flush() }
func (device sliceDevice) Capacity() uint64 { return device.slice.Length }

func openImage(t *testing.T, path string) fileDevice {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	info, _ := file.Stat()
	return fileDevice{file: file, size: uint64(info.Size())}
}

// chosen scans an image and returns the volume auto-selection picks, as the
// block service would.
func chosen(t *testing.T, device fileDevice, scheme partition.Scheme) (sliceDevice, *partition.Table) {
	t.Helper()
	var scanner partition.Scanner
	table := &partition.Table{}
	if status := scanner.Scan(device, device.size, 512, table); status != partition.StatusOK {
		t.Fatalf("scan: %v", status)
	}
	if table.Scheme != scheme || table.GPT.Degraded() {
		t.Fatalf("scheme %v, want %v; health %+v", table.Scheme, scheme, table.GPT)
	}
	volume, status := partition.Select(table, &partition.Selector{})
	if status != partition.StatusOK {
		t.Fatalf("select: %v", status)
	}
	return sliceDevice{device: device, slice: blockproto.Slice{Start: volume.Start, Length: volume.Length, Writable: true}}, table
}

func mustName(t *testing.T, value string) ramfs.Name {
	t.Helper()
	name, ok := ramfs.MakeName([]byte(value))
	if !ok {
		t.Fatalf("bad name %q", value)
	}
	return name
}

// writeFiles writes path -> content onto a mounted volume, creating
// directories.
func writeFiles(t *testing.T, fs *fat32.FileSystem, root ramfs.NodeID, files map[string]string) {
	t.Helper()
	for path, content := range files {
		parts := strings.Split(path, "/")
		node := root
		for _, part := range parts[:len(parts)-1] {
			next, status := fs.OpenName(node, mustName(t, part))
			if status == ramfs.StatusNotFound {
				next, status = fs.MkdirName(node, mustName(t, part))
			}
			if status != ramfs.StatusOK {
				t.Fatalf("%s: %v", path, status)
			}
			node = next
		}
		id, status := fs.CreateName(node, mustName(t, parts[len(parts)-1]))
		if status == ramfs.StatusOK {
			_, status = fs.Write(id, 0, []byte(content))
		}
		if status != ramfs.StatusOK {
			t.Fatalf("%s: %v", path, status)
		}
	}
}

func readFile(t *testing.T, fs *fat32.FileSystem, root ramfs.NodeID, path string) string {
	t.Helper()
	node := root
	for _, part := range strings.Split(path, "/") {
		next, status := fs.OpenName(node, mustName(t, part))
		if status != ramfs.StatusOK {
			t.Fatalf("open %s: %v", path, status)
		}
		node = next
	}
	stat, _ := fs.Stat(node)
	data := make([]byte, stat.Size)
	if count, status := fs.Read(node, 0, data); status != ramfs.StatusOK || uint64(count) != stat.Size {
		t.Fatalf("read %s: %d %v", path, count, status)
	}
	return string(data)
}

// hdiutil runs hdiutil, skipping the test where it is missing.
func hdiutil(t *testing.T, args ...string) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the host tools here are macOS's")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil")
	}
	output, err := exec.Command("hdiutil", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// hostImage has the host make a raw disk image of the given layout
// (GPTSPUD or MBRSPUD) holding a FAT32 volume.
func hostImage(t *testing.T, layout string) string {
	t.Helper()
	directory := t.TempDir()
	dmg := filepath.Join(directory, "disk.dmg")
	hdiutil(t, "create", "-size", "300m", "-layout", layout, "-fs", "MS-DOS FAT32", "-volname", "HOSTDISK", "-type", "UDIF", dmg)
	raw := filepath.Join(directory, "disk")
	hdiutil(t, "convert", dmg, "-format", "UDTO", "-o", raw)
	return raw + ".cdr"
}

// attached attaches an image, mounting its volumes or not, and returns the
// lines hdiutil printed; the image is detached when the test ends or when
// the returned function is called.
func attached(t *testing.T, path string, mount bool) ([]string, func()) {
	t.Helper()
	args := []string{"attach", "-imagekey", "diskimage-class=CRawDiskImage"}
	if !mount {
		args = append(args, "-nomount")
	}
	lines := strings.Split(strings.TrimSpace(hdiutil(t, append(args, path)...)), "\n")
	disk := strings.Fields(lines[0])[0]
	done := false
	detach := func() {
		if !done {
			done = true
			if exec.Command("hdiutil", "detach", disk).Run() != nil {
				exec.Command("hdiutil", "detach", "-force", disk).Run()
			}
		}
	}
	t.Cleanup(detach)
	return lines, detach
}

// partitionLine finds the attach line of the first partition: its device,
// its content description, and its mount point if mounted.
func partitionLine(t *testing.T, lines []string) (string, string, string) {
	t.Helper()
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		device := strings.TrimSpace(fields[0])
		if strings.HasSuffix(device, "s1") {
			content, mount := "", ""
			if len(fields) > 1 {
				content = strings.TrimSpace(fields[1])
			}
			if len(fields) > 2 {
				mount = strings.TrimSpace(fields[2])
			}
			return device, content, mount
		}
	}
	t.Fatalf("no partition in %q", lines)
	return "", "", ""
}

func hostCheckFAT(t *testing.T, device string) {
	t.Helper()
	tool, err := exec.LookPath("fsck_msdos")
	if err != nil {
		t.Log("no fsck_msdos")
		return
	}
	raw := strings.Replace(device, "/dev/disk", "/dev/rdisk", 1)
	if output, err := exec.Command(tool, "-n", raw).CombinedOutput(); err != nil {
		t.Fatalf("fsck_msdos %s: %v\n%s", raw, err, output)
	}
}

var unicodeFiles = map[string]string{
	"Документы/Фото с моря.txt": "море",
	"日本語/音乐.txt":                "音乐",
	"emoji-😀.txt":               "😀",
}

// TestHostMadeTables has the host partition and format disks, then reads
// their tables here, mounts the FAT32 partition through a block slice,
// writes to it, and has the host mount and check the result.
func TestHostMadeTables(t *testing.T) {
	for _, test := range []struct {
		layout string
		scheme partition.Scheme
	}{{"GPTSPUD", partition.SchemeGPT}, {"MBRSPUD", partition.SchemeMBR}} {
		t.Run(test.layout, func(t *testing.T) {
			path := hostImage(t, test.layout)
			device := openImage(t, path)
			volume, table := chosen(t, device, test.scheme)
			if table.Count != 1 || table.Partitions[0].Kind != partition.KindData || volume.slice.Start == 0 {
				t.Fatalf("table: %d partitions, %+v", table.Count, table.Partitions[0])
			}
			var fs fat32.FileSystem
			root, ok := fs.Mount(volume)
			if !ok {
				t.Fatalf("the host's FAT32 partition was refused: %v", fs.Refused())
			}
			writeFiles(t, &fs, root, unicodeFiles)
			fs.Flush()
			lines, detach := attached(t, path, true)
			partitionDevice, _, mount := partitionLine(t, lines)
			for name, content := range unicodeFiles {
				data, err := os.ReadFile(filepath.Join(mount, name))
				if err != nil || string(data) != content {
					t.Fatalf("the host read %s as %q: %v", name, data, err)
				}
			}
			detach()
			lines, _ = attached(t, path, false)
			if device, _, _ := partitionLine(t, lines); device != partitionDevice {
				t.Logf("partition device changed from %s to %s", partitionDevice, device)
			}
			partitionDevice, _, _ = partitionLine(t, lines)
			hostCheckFAT(t, partitionDevice)
		})
	}
}

// ourImage writes a table with WriteMBR or WriteGPT, formats its partition
// with fat32.Format, and fills it.
func ourImage(t *testing.T, scheme partition.Scheme) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 300 << 20
	file.Truncate(size)
	device := fileDevice{file: file, size: size}
	switch scheme {
	case partition.SchemeMBR:
		if !partition.WriteMBR(device, 512, []partition.MBRPartition{{Type: 0x0c, Start: 2048, Count: size/512 - 2048}}) {
			t.Fatal("write MBR")
		}
	case partition.SchemeGPT:
		_, last := partition.GPTUsable(size, 512)
		unique, _ := partition.ParseGUID("5E1F0000-0000-4000-8000-000000000001")
		disk, _ := partition.ParseGUID("5E1F0000-0000-4000-8000-000000000002")
		if !partition.WriteGPT(device, size, 512, disk, []partition.GPTPartition{
			{Type: partition.TypeBasicData, Unique: unique, First: 2048, Last: last, Name: "GoOS данные"}}) {
			t.Fatal("write GPT")
		}
	}
	volume, _ := chosen(t, device, scheme)
	if !fat32.Format(volume, fat32.Layout{SectorsPerCluster: 8, FATs: 2, VolumeID: 0x600d, HiddenSectors: 2048}) {
		t.Fatal("format")
	}
	var fs fat32.FileSystem
	root, ok := fs.Mount(volume)
	if !ok {
		t.Fatal("mount")
	}
	writeFiles(t, &fs, root, unicodeFiles)
	fs.Flush()
	file.Close()
	return path
}

// TestHostReadsOurTables has the host's own tools read the tables this code
// writes, mount the FAT32 volume inside, and check it.
func TestHostReadsOurTables(t *testing.T) {
	for _, test := range []struct {
		scheme  partition.Scheme
		content string
	}{{partition.SchemeGPT, "Microsoft Basic Data"}, {partition.SchemeMBR, "Windows_FAT_32"}} {
		t.Run(test.scheme.String(), func(t *testing.T) {
			path := ourImage(t, test.scheme)
			switch test.scheme {
			case partition.SchemeGPT:
				if tool, err := exec.LookPath("gpt"); err == nil {
					output, err := exec.Command(tool, "-r", "show", path).CombinedOutput()
					if err != nil || !strings.Contains(string(output), "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7") || !strings.Contains(string(output), "Sec GPT header") {
						t.Fatalf("gpt show: %v\n%s", err, output)
					}
				}
			case partition.SchemeMBR:
				if tool, err := exec.LookPath("fdisk"); err == nil {
					output, err := exec.Command(tool, path).CombinedOutput()
					if err != nil || !strings.Contains(string(output), "0C") || !strings.Contains(string(output), "2048") {
						t.Fatalf("fdisk: %v\n%s", err, output)
					}
				}
			}
			lines, _ := attached(t, path, false)
			device, content, _ := partitionLine(t, lines)
			if content != test.content {
				t.Fatalf("the host sees the partition as %q", content)
			}
			hostCheckFAT(t, device)
			// Detach before mounting: attach again with the volume mounted.
			exec.Command("hdiutil", "detach", strings.TrimSuffix(device, "s1")).Run()
			lines, _ = attached(t, path, true)
			_, _, mount := partitionLine(t, lines)
			for name, want := range unicodeFiles {
				data, err := os.ReadFile(filepath.Join(mount, name))
				if err != nil || string(data) != want {
					t.Fatalf("the host read %s as %q: %v", name, data, err)
				}
			}
		})
	}
}

// fixtureFiles is what TestMakePartitionFixtures has the host write.
var fixtureFiles = map[string][]byte{
	"Документы/Фото с моря.txt": []byte("море"),
	"Deep/Er/leaf.bin":          bytes.Repeat([]byte("leaf"), 3000),
	"README.TXT":                []byte("upper-case 8.3"),
}

// TestMakePartitionFixtures regenerates testdata/: disks partitioned and
// formatted by the host, with files the host wrote and a MANIFEST.TXT of
// "crc32<TAB>path" lines. It runs only when asked:
//
//	GOOS_PARTITION_FIXTURES=1 go test -run TestMakePartitionFixtures ./userspace/partition
func TestMakePartitionFixtures(t *testing.T) {
	if os.Getenv("GOOS_PARTITION_FIXTURES") != "1" {
		t.Skip("GOOS_PARTITION_FIXTURES=1 regenerates the fixtures")
	}
	for _, layout := range []string{"GPTSPUD", "MBRSPUD"} {
		path := hostImage(t, layout)
		lines, detach := attached(t, path, true)
		_, _, mount := partitionLine(t, lines)
		var names []string
		for name := range fixtureFiles {
			names = append(names, name)
		}
		sort.Strings(names)
		var manifest strings.Builder
		for _, name := range names {
			full := filepath.Join(mount, name)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, fixtureFiles[name], 0o644); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&manifest, "%08x\t%s\n", crc32.ChecksumIEEE(fixtureFiles[name]), name)
		}
		os.WriteFile(filepath.Join(mount, "MANIFEST.TXT"), []byte(manifest.String()), 0o644)
		detach()
		image, _ := os.ReadFile(path)
		name := "host-" + strings.ToLower(strings.TrimSuffix(layout, "SPUD")) + ".img.gz"
		if err := os.WriteFile(filepath.Join("testdata", name), imagefixture.Pack(image), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPartitionFixtures reads the host-made disks kept in testdata, so that
// foreign tables are covered on every host.
func TestPartitionFixtures(t *testing.T) {
	for name, scheme := range map[string]partition.Scheme{"host-gpt.img.gz": partition.SchemeGPT, "host-mbr.img.gz": partition.SchemeMBR} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := imagefixture.Unpack(filepath.Join("testdata", name), file); err != nil {
				t.Fatal(err)
			}
			file.Close()
			device := openImage(t, path)
			volume, _ := chosen(t, device, scheme)
			var fs fat32.FileSystem
			root, ok := fs.Mount(volume)
			if !ok {
				t.Fatalf("refused: %v", fs.Refused())
			}
			manifest := readFile(t, &fs, root, "MANIFEST.TXT")
			lines := strings.Split(strings.TrimSpace(manifest), "\n")
			if len(lines) != len(fixtureFiles) {
				t.Fatalf("manifest lists %d files", len(lines))
			}
			for _, line := range lines {
				fields := strings.SplitN(line, "\t", 2)
				if got := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(readFile(t, &fs, root, fields[1])))); got != fields[0] {
					t.Fatalf("%s: crc %s, manifest %s", fields[1], got, fields[0])
				}
			}
			// The volume is writable through its slice, and nothing lands
			// outside it.
			before := make([]byte, volume.slice.Start)
			device.ReadAt(0, before)
			writeFiles(t, &fs, root, map[string]string{"Сделано в GoOS/новый.txt": "новый", "big.bin": strings.Repeat("x", 300000)})
			after := make([]byte, volume.slice.Start)
			device.ReadAt(0, after)
			if !bytes.Equal(before, after) {
				t.Fatal("a write through the slice reached the table")
			}
		})
	}
}
