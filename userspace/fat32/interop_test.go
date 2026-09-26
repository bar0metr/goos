package fat32

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"goos/kernel/ramfs"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Interoperability with the host's own FAT tools. Every test here is
// conditional: it runs where the tools exist (newfs_msdos, fsck_msdos and
// hdiutil on macOS; mkfs.fat, fsck.fat and mtools on Linux) and is skipped
// elsewhere. testdata/ holds images made this way, so the foreign-volume
// cases also run on hosts without the tools (TestForeignFixtures).

// hostFormat formats the image at path with the host's formatter. It
// reports false, with the reason, when the host has none or refuses the
// geometry.
func hostFormat(t *testing.T, path string, sector, perCluster, fats int) (bool, string) {
	t.Helper()
	if tool, err := exec.LookPath("mkfs.fat"); err == nil {
		output, err := exec.Command(tool, "-F", "32", "-S", fmt.Sprint(sector), "-s", fmt.Sprint(perCluster),
			"-f", fmt.Sprint(fats), "-n", "HOSTFAT", path).CombinedOutput()
		return err == nil, string(output)
	}
	if runtime.GOOS != "darwin" {
		return false, "no mkfs.fat"
	}
	tool, err := exec.LookPath("newfs_msdos")
	if err != nil {
		return false, "no newfs_msdos"
	}
	// newfs_msdos needs a device, not a file: attach the image without
	// mounting it.
	device, detach, ok := attach(t, path, false)
	if !ok {
		return false, "hdiutil attach failed"
	}
	defer detach()
	raw := strings.Replace(device, "/dev/disk", "/dev/rdisk", 1)
	output, err := exec.Command(tool, "-F", "32", "-S", fmt.Sprint(sector), "-c", fmt.Sprint(perCluster),
		"-n", fmt.Sprint(fats), "-v", "HOSTFAT", raw).CombinedOutput()
	return err == nil, string(output)
}

// attach attaches an image with hdiutil, mounted or not, and returns its
// device node and mount point.
func attach(t *testing.T, path string, mount bool) (string, func(), bool) {
	t.Helper()
	args := []string{"attach", "-imagekey", "diskimage-class=CRawDiskImage"}
	if !mount {
		args = append(args, "-nomount")
	}
	output, err := exec.Command("hdiutil", append(args, path)...).CombinedOutput()
	if err != nil {
		t.Logf("hdiutil attach: %v\n%s", err, output)
		return "", nil, false
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	device := strings.Fields(lines[0])[0]
	detach := func() {
		if err := exec.Command("hdiutil", "detach", device).Run(); err != nil {
			exec.Command("hdiutil", "detach", "-force", device).Run()
		}
	}
	if !mount {
		return device, detach, true
	}
	for _, line := range lines {
		if fields := strings.Split(line, "\t"); len(fields) > 2 && strings.HasPrefix(strings.TrimSpace(fields[len(fields)-1]), "/") {
			return strings.TrimSpace(fields[len(fields)-1]), detach, true
		}
	}
	detach()
	return "", nil, false
}

// hostMount mounts the image with the host's own FAT driver and returns the
// directory it is at, or false when the host cannot. Only macOS can here.
func hostMount(t *testing.T, path string) (string, func(), bool) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return "", nil, false
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		return "", nil, false
	}
	return attach(t, path, true)
}

// hostCheck runs the host's FAT checker without repairing. It reports false
// when there is none.
func hostCheck(t *testing.T, path string) bool {
	t.Helper()
	var command []string
	for _, candidate := range [][]string{{"fsck.fat", "-n", "-V"}, {"fsck_msdos", "-n"}} {
		if tool, err := exec.LookPath(candidate[0]); err == nil {
			command = append([]string{tool}, candidate[1:]...)
			break
		}
	}
	if command == nil {
		return false
	}
	output, err := exec.Command(command[0], append(command[1:], path)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(command[0]), err, output)
	}
	return true
}

// imageFile creates a sparse image file of size bytes.
func imageFile(t *testing.T, size uint64) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "volume.img")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	return path, file
}

func openDevice(t *testing.T, path string) fileDevice {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	info, _ := file.Stat()
	return fileDevice{file: file, size: uint64(info.Size())}
}

// externalGeometries have enough clusters that every formatter calls the
// volume FAT32.
var externalGeometries = []struct {
	sector, perCluster, fats int
	size                     uint64
}{
	{512, 1, 2, 40 << 20},
	{512, 1, 1, 40 << 20},
	{512, 8, 2, 300 << 20},
	{1024, 2, 2, 300 << 20},
	{2048, 1, 2, 160 << 20},
	{4096, 1, 2, 300 << 20},
	{4096, 1, 1, 300 << 20},
}

// TestExternallyFormattedVolumes formats volumes of each geometry with the
// host's formatter, then mounts, fills and checks them here, and has the
// host's checker accept the result.
func TestExternallyFormattedVolumes(t *testing.T) {
	ran := false
	for _, geometry := range externalGeometries {
		name := fmt.Sprintf("%dB-sectors-%dx-%dfat", geometry.sector, geometry.perCluster, geometry.fats)
		t.Run(name, func(t *testing.T) {
			path, file := imageFile(t, geometry.size)
			file.Close()
			if ok, why := hostFormat(t, path, geometry.sector, geometry.perCluster, geometry.fats); !ok {
				t.Skipf("host formatter: %s", why)
			}
			ran = true
			device := openDevice(t, path)
			var fs FileSystem
			if _, ok := fs.Mount(device); !ok {
				t.Fatalf("a host-formatted volume was refused: %v", fs.Refused())
			}
			if fs.sectorSize != uint64(geometry.sector) || fs.clusterSize != uint64(geometry.sector*geometry.perCluster) || fs.fatTotal != uint32(geometry.fats) {
				t.Fatalf("geometry %d/%d/%d", fs.sectorSize, fs.clusterSize, fs.fatTotal)
			}
			if health := fs.Health(); health.Degraded() && health.BackupFSInfo != BackupFSInfoDiffers {
				t.Fatalf("health %+v", health)
			}
			check(t, device)
			exercise(t, device)
			filled, root := mounted(t, device)
			docs := mkdir(t, filled, root, "Документы")
			for i, name := range unicodeNames {
				writeFile(t, filled, docs, name, fmt.Sprint(i))
			}
			big, _ := filled.CreateName(docs, mustName(t, "большой файл.bin"))
			writeChunks(t, filled, big, 0, pattern(16<<20+3, 9), 1<<20)
			check(t, device)
			if !hostCheck(t, path) {
				t.Log("no host checker")
			}
		})
	}
	if !ran {
		t.Skip("no host FAT formatter")
	}
}

// TestHostAndGoOSReadEachOthersFiles has the host's FAT driver write Unicode
// names, nested directories and a large file; reads them here, timestamps
// included; changes the volume here; then has the host read back what this
// code wrote and its checker accept it.
func TestHostAndGoOSReadEachOthersFiles(t *testing.T) {
	path, file := imageFile(t, 40<<20)
	file.Close()
	if ok, why := hostFormat(t, path, 512, 1, 2); !ok {
		t.Skipf("host formatter: %s", why)
	}
	mount, unmount, ok := hostMount(t, path)
	if !ok {
		t.Skip("the host cannot mount FAT images")
	}
	large := pattern(3<<20+11, 7)
	written := map[string][]byte{}
	for i, name := range unicodeNames {
		written[filepath.Join("Документы", name)] = []byte(fmt.Sprint("host ", i))
	}
	written["Nested/Deeper/Файл.txt"] = []byte("deep")
	written["README.TXT"] = []byte("upper")
	written["lower.txt"] = []byte("lower")
	written["large.bin"] = large
	for name, data := range written {
		full := filepath.Join(mount, name)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, data, 0o644); err != nil {
			unmount()
			t.Fatal(err)
		}
	}
	unmount()

	device := openDevice(t, path)
	fs, root := mounted(t, device)
	for name, data := range written {
		node := root
		for _, part := range strings.Split(name, "/") {
			node = mustOpen(t, fs, node, part)
		}
		if got := readAll(t, fs, node, len(data), 65536); !bytes.Equal(got, data) {
			t.Fatalf("%s: read differs", name)
		}
		if value := metadata(t, fs, node); !value.Modified.Known || !value.Created.Known {
			t.Fatalf("%s: host timestamps not read: %+v", name, value)
		}
	}
	// macOS adds AppleDouble "._" companions for extended attributes.
	listed := withoutAppleDouble(listNames(t, fs, mustOpen(t, fs, root, "Документы")))
	sort.Strings(listed)
	want := append([]string(nil), unicodeNames...)
	sort.Strings(want)
	if strings.Join(listed, "|") != strings.Join(want, "|") {
		t.Fatalf("host names listed as %q", listed)
	}
	// 8.3 names keep the case the host gave them.
	rootNames := strings.Join(listNames(t, fs, root), "|")
	if !strings.Contains(rootNames, "README.TXT") || !strings.Contains(rootNames, "lower.txt") {
		t.Fatalf("root lists %s", rootNames)
	}
	check(t, device)

	// Now change it here.
	ours := mkdir(t, fs, root, "Сделано в GoOS")
	for i, name := range unicodeNames {
		writeFile(t, fs, ours, name, fmt.Sprint("goos ", i))
	}
	writeFile(t, fs, ours, "MixedCase Name.Txt", "mixed")
	fs.Truncate(mustOpen(t, fs, root, "large.bin"), 1<<20)
	fs.UnlinkName(mustOpen(t, fs, mustOpen(t, fs, root, "Nested"), "Deeper"), mustName(t, "файл.txt"))
	fs.RenameName(mustOpen(t, fs, root, "lower.txt"), ours, mustName(t, "Перемещённый.txt"))
	check(t, device)
	device.file.Sync()
	hostCheck(t, path)

	mount, unmount, ok = hostMount(t, path)
	if !ok {
		t.Fatal("the host could not remount the volume")
	}
	defer unmount()
	entries, err := os.ReadDir(filepath.Join(mount, "Сделано в GoOS"))
	if err != nil {
		t.Fatal(err)
	}
	var hostNames []string
	for _, entry := range entries {
		hostNames = append(hostNames, entry.Name())
	}
	hostNames = withoutAppleDouble(hostNames)
	// macOS lists names decomposed (NFD) whatever is on disk, and looks
	// them up either way, so the names are compared by lookup.
	want = append(append([]string(nil), unicodeNames...), "MixedCase Name.Txt", "Перемещённый.txt")
	if len(hostNames) != len(want) {
		t.Fatalf("the host lists %q, want %q", hostNames, want)
	}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(mount, "Сделано в GoOS", name)); err != nil {
			t.Fatalf("the host cannot find %q among %q: %v", name, hostNames, err)
		}
	}
	for i, name := range unicodeNames {
		data, err := os.ReadFile(filepath.Join(mount, "Сделано в GoOS", name))
		if err != nil || string(data) != fmt.Sprint("goos ", i) {
			t.Fatalf("host read %s: %q %v", name, data, err)
		}
	}
	if info, err := os.Stat(filepath.Join(mount, "large.bin")); err != nil || info.Size() != 1<<20 {
		t.Fatalf("host sees large.bin as %v %v", info, err)
	}
}

// TestHostCheckerAcceptsEveryGeometryWeFormat runs the host checker over
// volumes this code formatted and filled, in every sector size.
func TestHostCheckerAcceptsEveryGeometryWeFormat(t *testing.T) {
	if _, err := exec.LookPath("fsck_msdos"); err != nil {
		if _, err := exec.LookPath("fsck.fat"); err != nil {
			t.Skip("no host FAT checker")
		}
	}
	for _, sector := range sectorSizes {
		for _, fats := range []uint8{1, 2} {
			t.Run(fmt.Sprintf("%d-%d", sector, fats), func(t *testing.T) {
				path, file := imageFile(t, 300<<20)
				device := fileDevice{file: file, size: 300 << 20}
				if !Format(device, Layout{BytesPerSector: sector, SectorsPerCluster: 1, FATs: fats, VolumeID: 0x600d}) {
					t.Fatal("format")
				}
				exercise(t, device)
				fs, root := mounted(t, device)
				for i, name := range unicodeNames {
					writeFile(t, fs, root, name, fmt.Sprint(i))
				}
				file.Sync()
				hostCheck(t, path)
			})
		}
	}
}

// fixtureFiles is what TestMakeForeignFixtures has the host write.
var fixtureFiles = map[string][]byte{
	"Документы/Фото с моря.jpg":   pattern(9000, 1),
	"Документы/日本語.txt":           []byte("日本語のテキスト"),
	"Музыка/音乐.mp3":               pattern(40000, 2),
	"emoji-😀.txt":                 []byte("😀"),
	"Deep/Er/And Deeper/leaf.txt": []byte("leaf"),
	"README.TXT":                  []byte("upper-case 8.3"),
	"lower.txt":                   []byte("lower-case 8.3"),
	"Mixed Case Long Name.Text":   pattern(5000, 3),
}

// TestMakeForeignFixtures regenerates testdata/: volumes formatted and
// written by the host's own FAT implementation, with a MANIFEST.TXT of
// "crc32<TAB>path" lines. It runs only when asked:
//
//	GOOS_FAT32_FIXTURES=1 go test -run TestMakeForeignFixtures ./userspace/fat32
func TestMakeForeignFixtures(t *testing.T) {
	if os.Getenv("GOOS_FAT32_FIXTURES") != "1" {
		t.Skip("GOOS_FAT32_FIXTURES=1 regenerates the fixtures")
	}
	// The macOS driver mounts only 512-byte logical sectors on an image, so
	// the fixtures vary cluster size and FAT count instead.
	for _, fixture := range []struct {
		name                     string
		sector, perCluster, fats int
		size                     uint64
	}{
		{"host-512b-1x-2fat", 512, 1, 2, 34 << 20},
		{"host-512b-8x-1fat", 512, 8, 1, 270 << 20},
	} {
		path, file := imageFile(t, fixture.size)
		file.Close()
		if ok, why := hostFormat(t, path, fixture.sector, fixture.perCluster, fixture.fats); !ok {
			t.Fatalf("host formatter: %s", why)
		}
		mount, unmount, ok := hostMount(t, path)
		if !ok {
			t.Fatal("the host cannot mount FAT images")
		}
		var manifest strings.Builder
		var names []string
		for name := range fixtureFiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			full := filepath.Join(mount, name)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, fixtureFiles[name], 0o644); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&manifest, "%08x\t%s\n", crc32Of(fixtureFiles[name]), name)
		}
		os.WriteFile(filepath.Join(mount, "MANIFEST.TXT"), []byte(manifest.String()), 0o644)
		unmount()
		image, _ := os.ReadFile(path)
		if err := os.WriteFile(filepath.Join("testdata", fixture.name+".img.gz"), packImage(image), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestForeignFixtures mounts the host-made images kept in testdata, so that
// volumes from other systems are covered on every host.
func TestForeignFixtures(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.img.gz")
	if len(paths) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			image, file := imageFile(t, 0)
			unpackImage(t, path, file)
			file.Close()
			device := openDevice(t, image)
			fs, root := mounted(t, device)
			if health := fs.Health(); health.Degraded() && health.BackupFSInfo != BackupFSInfoDiffers {
				t.Fatalf("health %+v", health)
			}
			manifestNode := mustOpen(t, fs, root, "MANIFEST.TXT")
			stat, _ := fs.Stat(manifestNode)
			manifest := string(readAll(t, fs, manifestNode, int(stat.Size), 4096))
			for _, line := range strings.Split(strings.TrimSpace(manifest), "\n") {
				fields := strings.SplitN(line, "\t", 2)
				node := root
				for _, part := range strings.Split(fields[1], "/") {
					node = mustOpen(t, fs, node, part)
				}
				stat, _ := fs.Stat(node)
				data := readAll(t, fs, node, int(stat.Size), 65536)
				if got := fmt.Sprintf("%08x", crc32Of(data)); got != fields[0] {
					t.Fatalf("%s: crc %s, manifest %s", fields[1], got, fields[0])
				}
				if value := metadata(t, fs, node); !value.Modified.Known {
					t.Fatalf("%s: no modification time", fields[1])
				}
			}
			if lines := strings.Count(manifest, "\n"); lines != len(fixtureFiles) {
				t.Fatalf("manifest lists %d files", lines)
			}
			check(t, device)
			exercise(t, device)
		})
	}
}

// A fixture is stored sparse, since deflate compresses long runs of zeros
// only about a thousandfold: gzip of "GOOSFIX1", the image size, then each
// 4 KiB block that is not all zeros as its offset and its bytes.
const fixtureBlock = 4096

func packImage(image []byte) []byte {
	var packed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	var header [16]byte
	copy(header[:8], "GOOSFIX1")
	binary.LittleEndian.PutUint64(header[8:], uint64(len(image)))
	writer.Write(header[:])
	zero := make([]byte, fixtureBlock)
	for offset := 0; offset < len(image); offset += fixtureBlock {
		block := image[offset:min(offset+fixtureBlock, len(image))]
		if bytes.Equal(block, zero[:len(block)]) {
			continue
		}
		var at [8]byte
		binary.LittleEndian.PutUint64(at[:], uint64(offset))
		writer.Write(at[:])
		writer.Write(append(block, zero[len(block):]...))
	}
	writer.Close()
	return packed.Bytes()
}

// unpackImage writes the fixture at path into file, which it sizes first.
func unpackImage(t *testing.T, path string, file *os.File) {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	reader, err := gzip.NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil || string(header[:8]) != "GOOSFIX1" {
		t.Fatalf("%s is not a fixture: %v", path, err)
	}
	size := int64(binary.LittleEndian.Uint64(header[8:]))
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 8+fixtureBlock)
	for {
		if _, err := io.ReadFull(reader, block); err == io.EOF {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		offset := int64(binary.LittleEndian.Uint64(block[:8]))
		length := min(int64(fixtureBlock), size-offset)
		if _, err := file.WriteAt(block[8:8+length], offset); err != nil {
			t.Fatal(err)
		}
	}
}

func crc32Of(data []byte) uint32 { return crc32.ChecksumIEEE(data) }

func withoutAppleDouble(names []string) []string {
	var kept []string
	for _, name := range names {
		if !strings.HasPrefix(name, "._") {
			kept = append(kept, name)
		}
	}
	return kept
}

// TestHostFormattedVolumeFilledAndFragmented fills a host-formatted volume
// with interleaved, fragmenting writes until it is nearly full, then full,
// removes every other file and refills the holes, and has the host checker
// accept each state.
func TestHostFormattedVolumeFilledAndFragmented(t *testing.T) {
	path, file := imageFile(t, 40<<20)
	file.Close()
	if ok, why := hostFormat(t, path, 512, 1, 2); !ok {
		t.Skipf("host formatter: %s", why)
	}
	device := openDevice(t, path)
	fs, root := mounted(t, device)
	const files = 8
	ids := make([]ramfs.NodeID, files)
	sizes := make([]uint64, files)
	for i := range ids {
		ids[i] = writeFile(t, fs, root, fmt.Sprintf("Заполнение %d.bin", i), "")
	}
	// An odd size keeps the chains' ends away from cluster boundaries.
	chunk := make([]byte, 4001)
	full, nearlyFull := false, false
	for round := 0; !full; round++ {
		for i, id := range ids {
			for j := range chunk {
				chunk[j] = byte(round + i + j)
			}
			if _, status := fs.Write(id, sizes[i], chunk); status == ramfs.StatusNoSpace {
				full = true
				break
			} else if status != ramfs.StatusOK {
				t.Fatal(status)
			}
			sizes[i] += uint64(len(chunk))
		}
		if free, _ := fs.FreeClusters(); !nearlyFull && free < 200 && free > 0 {
			nearlyFull = true
			check(t, device)
			hostCheck(t, path)
		}
	}
	// A write that does not fit is refused whole; smaller ones take the rest.
	for {
		if _, status := fs.Write(ids[0], sizes[0], chunk[:512]); status != ramfs.StatusOK {
			break
		}
		sizes[0] += 512
	}
	if free, _ := fs.FreeClusters(); free != 0 {
		t.Fatalf("%d clusters free on a full volume", free)
	}
	check(t, device)
	hostCheck(t, path)
	for i := 0; i < files; i += 2 {
		if status := fs.UnlinkName(root, mustName(t, fmt.Sprintf("Заполнение %d.bin", i))); status != ramfs.StatusOK {
			t.Fatal(status)
		}
	}
	refill, _ := fs.CreateName(root, mustName(t, "Refill.bin"))
	data := pattern(int(sizes[0]+sizes[2]), 5)
	writeChunks(t, fs, refill, 0, data, 64<<10)
	fs, root = mounted(t, device)
	if got := readAll(t, fs, mustOpen(t, fs, root, "refill.bin"), len(data), 64<<10); !bytes.Equal(got, data) {
		t.Fatal("refill differs")
	}
	for i := 1; i < files; i += 2 {
		stat, _ := fs.Stat(mustOpen(t, fs, root, fmt.Sprintf("Заполнение %d.bin", i)))
		if stat.Size != sizes[i] {
			t.Fatalf("file %d is %d bytes, want %d", i, stat.Size, sizes[i])
		}
	}
	check(t, device)
	hostCheck(t, path)
}
