package ext4

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// memDevice is a volume held in memory.
type memDevice struct {
	data   []byte
	reads  int
	failAt int64
}

func (device *memDevice) ReadAt(offset uint64, target []byte) bool {
	device.reads++
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	if device.failAt >= 0 && int64(offset) <= device.failAt && device.failAt < int64(end) {
		return false
	}
	copy(target, device.data[offset:end])
	return true
}

func (device *memDevice) WriteAt(uint64, []byte) bool { return false }
func (device *memDevice) Flush() bool                 { return true }
func (device *memDevice) Capacity() uint64            { return uint64(len(device.data)) }

var (
	fixtureMu    sync.Mutex
	fixtureCache = map[string][]byte{}
)

// fixture returns a fresh copy of a pinned image after checking it against
// testdata/SHA256SUMS.
func fixture(t testing.TB, name string) *memDevice {
	t.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	image, ok := fixtureCache[name]
	if !ok {
		path := filepath.Join("testdata", name+".img.gz")
		compressed, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(compressed)
		if want := pinnedSum(t, name+".img.gz"); hex.EncodeToString(sum[:]) != want {
			t.Fatalf("%s does not match its pinned checksum", path)
		}
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		if image, err = io.ReadAll(reader); err != nil {
			t.Fatal(err)
		}
		fixtureCache[name] = image
	}
	return &memDevice{data: append([]byte(nil), image...), failAt: -1}
}

func pinnedSum(t testing.TB, file string) string {
	t.Helper()
	sums, err := os.ReadFile(filepath.Join("testdata", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == file {
			return fields[0]
		}
	}
	t.Fatalf("%s has no pinned checksum", file)
	return ""
}

func mount(t testing.TB, device Device) *FS {
	t.Helper()
	fs, err := Mount(device, Options{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	return fs
}

// resolve walks an absolute path without following symlinks.
func resolve(t testing.TB, fs *FS, path string) *Inode {
	t.Helper()
	inode, err := fs.Root()
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		number, _, err := fs.Lookup(inode, []byte(part))
		if err != nil {
			t.Fatalf("lookup %q in %s: %v", part, path, err)
		}
		if inode, err = fs.Inode(number); err != nil {
			t.Fatalf("inode %d (%s): %v", number, path, err)
		}
	}
	return inode
}

type manifestEntry struct {
	kind, path, target string
	size               int64
	sum                string
}

func readManifest(t testing.TB, name string) []manifestEntry {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var entries []manifestEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		entry := manifestEntry{kind: fields[0], path: strings.TrimPrefix(fields[1], ".")}
		switch entry.kind {
		case "symlink", "device":
			entry.target = fields[2]
		case "file":
			entry.size, _ = strconv.ParseInt(fields[2], 10, 64)
			entry.sum = fields[3]
		}
		entries = append(entries, entry)
	}
	return entries
}

func readAll(t testing.TB, fs *FS, inode *Inode) []byte {
	t.Helper()
	data := make([]byte, inode.Size)
	count, err := fs.ReadFile(inode, 0, data)
	if err != nil || uint64(count) != inode.Size {
		t.Fatalf("read inode %d: %d of %d: %v", inode.Number, count, inode.Size, err)
	}
	return data
}

// fixtureManifests pairs each image with the tree mke2fs copied into it.
var fixtureManifests = []struct{ image, manifest string }{
	{"canonical", "manifest.tsv"},
	{"legacy", "manifest.tsv"},
	{"hash-tea", "hash.manifest.tsv"},
	{"hash-legacy", "hash.manifest.tsv"},
	{"blockmap-1k", "blockmap.manifest.tsv"},
	{"links", "links.manifest.tsv"},
}

// TestFixturesMatchManifest compares every name, type, size, content hash
// and link target Linux wrote with what the reader sees.
func TestFixturesMatchManifest(t *testing.T) {
	for _, pair := range fixtureManifests {
		t.Run(pair.image, func(t *testing.T) {
			fs := mount(t, fixture(t, pair.image))
			for _, entry := range readManifest(t, pair.manifest) {
				inode := resolve(t, fs, entry.path)
				switch entry.kind {
				case "dir":
					if !inode.IsDir() {
						t.Fatalf("%s: not a directory", entry.path)
					}
				case "fifo":
					if inode.Type() != ModeFIFO {
						t.Fatalf("%s: mode %#o", entry.path, inode.Mode)
					}
				case "device":
					// stat -c %t:%T prints major and minor in hex.
					var major, minor uint32
					fmt.Sscanf(entry.target, "%x:%x", &major, &minor)
					want := minor&0xff | major<<8 | (minor&^0xff)<<12
					if inode.Type() != ModeChar && inode.Type() != ModeBlock || inode.DeviceNumber() != want {
						t.Fatalf("%s: device %#x, want %#x", entry.path, inode.DeviceNumber(), want)
					}
				case "symlink":
					target, err := fs.Readlink(inode)
					if err != nil || string(target) != entry.target {
						t.Fatalf("%s: readlink %q, %v", entry.path, target, err)
					}
				case "file":
					if !inode.IsRegular() || int64(inode.Size) != entry.size {
						t.Fatalf("%s: size %d, want %d", entry.path, inode.Size, entry.size)
					}
					if entry.path == "/sparse" {
						continue // checked by TestSparseFile without hashing 5 GiB
					}
					sum := sha256.Sum256(readAll(t, fs, inode))
					if hex.EncodeToString(sum[:]) != entry.sum {
						t.Fatalf("%s: content differs", entry.path)
					}
				}
			}
		})
	}
}

// TestLinksMetadata compares the mode, owner and link count stat printed for
// every path of the links tree with the inode the reader decodes.
func TestLinksMetadata(t *testing.T) {
	fs := mount(t, fixture(t, "links"))
	data, err := os.ReadFile(filepath.Join("testdata", "links.metadata.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 60 {
		t.Fatalf("%d metadata lines", len(lines))
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		mode, _ := strconv.ParseUint(fields[1], 16, 16)
		uid, _ := strconv.ParseUint(fields[2], 10, 32)
		gid, _ := strconv.ParseUint(fields[3], 10, 32)
		links, _ := strconv.ParseUint(fields[4], 10, 16)
		inode := resolve(t, fs, strings.TrimPrefix(fields[0], "."))
		if uint64(inode.Mode) != mode || uint64(inode.UID) != uid || uint64(inode.GID) != gid || uint64(inode.Links) != links {
			t.Fatalf("%s: mode %#x uid %d gid %d links %d, Linux printed %s", fields[0], inode.Mode, inode.UID, inode.GID, inode.Links, line)
		}
	}
}

// TestHashVariants looks up every name of the indexed directory on volumes
// whose index uses each hash Linux offers, with names whose bytes above
// 0x7f make the signed variants differ from the unsigned ones.
func TestHashVariants(t *testing.T) {
	for _, tc := range []struct {
		image   string
		version uint8
	}{{"hash-tea", hashTEA}, {"hash-legacy", hashLegacy}, {"canonical", hashHalfMD4}} {
		t.Run(tc.image, func(t *testing.T) {
			fs := mount(t, fixture(t, tc.image))
			path := "/names"
			if tc.image == "canonical" {
				path = "/big"
			}
			dir := resolve(t, fs, path)
			root, err := fs.dirBlock(dir, 0)
			if err != nil || !fs.isIndexed(dir) {
				t.Fatalf("directory not indexed: %v", err)
			}
			if version, _, err := fs.dxRoot(dir, root); err != nil || version != tc.version {
				t.Fatalf("hash version %d, want %d: %v", version, tc.version, err)
			}
			count := 0
			_, err = fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
				count++
				name := append([]byte(nil), entry.Name()...)
				want := entry.Inode
				number, _, err := fs.Lookup(dir, name)
				if err != nil || number != want {
					t.Fatalf("lookup %q: %d, %v; want %d", name, number, err, want)
				}
				return true
			})
			if err != nil || count < 1200 && tc.image != "canonical" {
				t.Fatalf("listed %d names: %v", count, err)
			}
		})
	}
}

func TestBlockMappedVolume(t *testing.T) {
	fs := mount(t, fixture(t, "blockmap-1k"))
	if fs.blockSize != 1024 || fs.sb.FirstDataBlock != 1 || fs.groups != 8 {
		t.Fatalf("geometry: block %d, first %d, groups %d", fs.blockSize, fs.sb.FirstDataBlock, fs.groups)
	}
	if status := fs.Status(); status.Negotiation.Support != ReadOnly {
		t.Fatalf("ext3-style volume negotiated %v", status.Negotiation.Support)
	}
	inode := resolve(t, fs, "/indirect")
	if inode.Flags&inodeFlagExtents != 0 {
		t.Fatal("/indirect is extent-mapped; the fixture lost its shape")
	}
	tail := make([]byte, 4)
	if count, err := fs.ReadFile(inode, 70000*1024, tail); err != nil || count != 4 || string(tail) != "tail" {
		t.Fatalf("triple-indirect tail: %q %d %v", tail, count, err)
	}
}

// TestReadDirMatchesManifest lists every directory and compares the names
// with the manifest, including names that are not valid UTF-8.
func TestReadDirMatchesManifest(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	want := map[string]map[string]bool{}
	for _, entry := range readManifest(t, "manifest.tsv") {
		parent, base := filepath.Split(entry.path)
		parent = strings.TrimSuffix(parent, "/")
		if parent == "" {
			parent = "/"
		}
		if want[parent] == nil {
			want[parent] = map[string]bool{}
		}
		want[parent][base] = true
	}
	want["/"]["lost+found"] = true
	for dir, names := range want {
		inode := resolve(t, fs, dir)
		got := map[string]bool{}
		_, err := fs.ReadDir(inode, 0, func(entry *DirEntry) bool {
			if got[string(entry.Name())] {
				t.Fatalf("%s: %q listed twice", dir, entry.Name())
			}
			got[string(entry.Name())] = true
			return true
		})
		if err != nil {
			t.Fatalf("readdir %s: %v", dir, err)
		}
		if len(got) != len(names) {
			t.Fatalf("%s: %d names, want %d", dir, len(got), len(names))
		}
		for name := range names {
			if !got[name] {
				t.Fatalf("%s: %q missing", dir, name)
			}
		}
	}
}

func TestIndexedDirectoryLookup(t *testing.T) {
	for _, name := range []string{"canonical", "legacy"} {
		t.Run(name, func(t *testing.T) {
			fs := mount(t, fixture(t, name))
			big := resolve(t, fs, "/big")
			if !fs.isIndexed(big) {
				t.Fatal("/big is not hash-indexed; the fixture lost its index")
			}
			for index := 0; index < 2000; index++ {
				entryName := []byte("file-" + strconv.Itoa(index))
				number, fileType, err := fs.Lookup(big, entryName)
				if err != nil {
					t.Fatalf("lookup %s: %v", entryName, err)
				}
				if fileType != FileTypeRegular {
					t.Fatalf("%s: file type %d", entryName, fileType)
				}
				inode, err := fs.Inode(number)
				if err != nil || inode.Size != uint64(len("entry \n")+len(strconv.Itoa(index))) {
					t.Fatalf("%s: inode %d: %v", entryName, number, err)
				}
			}
			for _, missing := range []string{"file-2000", "file-", "FILE-1", "file-01"} {
				if _, _, err := fs.Lookup(big, []byte(missing)); KindOf(err) != KindNotFound {
					t.Fatalf("lookup %s: %v, want not found", missing, err)
				}
			}
		})
	}
}

// TestReadDirCursor resumes a listing at every returned cursor and checks
// that the pages add up to one full listing.
func TestReadDirCursor(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	big := resolve(t, fs, "/big")
	seen := map[string]bool{}
	cursor := uint64(0)
	for pages := 0; cursor < big.Size; pages++ {
		if pages > 10000 {
			t.Fatal("cursor does not advance")
		}
		taken := 0
		next, err := fs.ReadDir(big, cursor, func(entry *DirEntry) bool {
			if taken == 7 {
				return false
			}
			taken++
			seen[string(entry.Name())] = true
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if next <= cursor && next < big.Size {
			t.Fatalf("cursor went from %d to %d", cursor, next)
		}
		cursor = next
	}
	if len(seen) != 2000 {
		t.Fatalf("paged listing saw %d names", len(seen))
	}
}

func TestSparseFile(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	inode := resolve(t, fs, "/sparse")
	if inode.Size != 5368709121 {
		t.Fatalf("size %d", inode.Size)
	}
	check := func(offset uint64, want []byte) {
		got := make([]byte, len(want))
		if count, err := fs.ReadFile(inode, offset, got); err != nil || count != len(want) || !bytes.Equal(got, want) {
			t.Fatalf("at %d: %q %d %v", offset, got, count, err)
		}
	}
	check(0, bytes.Repeat([]byte{'A'}, 4096))
	check(4096, make([]byte, 8192))
	check(1048575, []byte{0, 'B', 0})
	check(4294967296-2, make([]byte, 4))
	check(5368709120-5000, append(make([]byte, 5000), 'C'))
	tail := make([]byte, 10)
	if count, err := fs.ReadFile(inode, 5368709120, tail); err != nil || count != 1 || tail[0] != 'C' {
		t.Fatalf("tail read %d %v", count, err)
	}
	if count, err := fs.ReadFile(inode, 5368709121, tail); err != nil || count != 0 {
		t.Fatalf("read at EOF %d %v", count, err)
	}
	// The reader allocates nothing for a hole: one 64 KiB read in the
	// middle of the gigabyte-sized hole touches only extent metadata.
	device := fs.device.(*memDevice)
	before := device.reads
	check(3<<30, make([]byte, 65536))
	if device.reads-before > 4 {
		t.Fatalf("hole read issued %d device reads", device.reads-before)
	}
}

func TestExtentTreeDepth(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	inode := resolve(t, fs, "/fragmented")
	if depth := le16(inode.blockArea[:], 6); depth == 0 {
		t.Fatal("/fragmented has a depth-0 extent tree; the fixture lost its shape")
	}
	block := make([]byte, 4096)
	for index := 0; index < 400; index++ {
		if _, err := fs.ReadFile(inode, uint64(index)*8192, block); err != nil {
			t.Fatal(err)
		}
		if want := "block " + leftPad(index); string(block[:len(want)]) != want {
			t.Fatalf("block %d: %q", index, block[:10])
		}
		if _, err := fs.ReadFile(inode, uint64(index)*8192+4096, block); err != nil {
			t.Fatal(err)
		}
		if index < 399 && !bytes.Equal(block, make([]byte, 4096)) {
			t.Fatalf("hole after block %d is not zero", index)
		}
	}
}

func leftPad(value int) string {
	text := strconv.Itoa(value)
	return strings.Repeat("0", 4-len(text)) + text
}

func TestMetadata(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	hello := resolve(t, fs, "/hello.txt")
	link := resolve(t, fs, "/link-to-hello")
	if hello.Number != link.Number || hello.Links != 2 {
		t.Fatalf("hard link: inodes %d/%d, links %d", hello.Number, link.Number, hello.Links)
	}
	if hello.Permissions() != 0644 || hello.UID != 0 || hello.GID != 0 {
		t.Fatalf("mode %o uid %d gid %d", hello.Permissions(), hello.UID, hello.GID)
	}
	if hello.Modify.Seconds != 1767225600 || !hello.HasCreate {
		t.Fatalf("mtime %d, crtime present %v", hello.Modify.Seconds, hello.HasCreate)
	}
	root, _ := fs.Root()
	if root.Links < 3 {
		t.Fatalf("root has %d links", root.Links)
	}
	if sym := resolve(t, fs, "/sym-fast"); sym.Flags&inodeFlagExtents != 0 && sym.Blocks512 != 0 {
		t.Fatal("/sym-fast is not a fast symlink")
	}
	info := fs.Info()
	if !info.ReadOnly || info.BlockSize != 4096 || info.Blocks != 16384 || string(info.Label) != "canonical" {
		t.Fatalf("info %+v", info)
	}
}

func TestXattrs(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	attrs, err := fs.Xattrs(resolve(t, fs, "/hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Name)] = string(attr.Value)
	}
	if got["user.small"] != "inline-value" || got["user.large"] != strings.Repeat("x", 600) {
		t.Fatalf("attributes %q", got)
	}
}

func TestUnwrittenExtentBeyondSize(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	inode := resolve(t, fs, "/empty")
	if inode.Size != 0 {
		t.Fatalf("size %d", inode.Size)
	}
	mapping, err := fs.mapBlock(inode, 3)
	if err != nil || !mapping.Unwritten {
		t.Fatalf("mapping %+v %v", mapping, err)
	}
	if count, err := fs.ReadFile(inode, 0, make([]byte, 100)); count != 0 || err != nil {
		t.Fatalf("read %d %v", count, err)
	}
}

func TestStatus(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	status := fs.Status()
	if status.State != StateReadyRO || !status.Journal.Present || !status.Journal.Clean() {
		t.Fatalf("status %+v", status)
	}
	if status.Negotiation.Support != ReadWrite {
		t.Fatalf("canonical profile negotiated %v: %s", status.Negotiation.Support, status.Negotiation.Reason)
	}
	legacy := mount(t, fixture(t, "legacy")).Status()
	if legacy.Negotiation.Support != ReadOnly || !strings.Contains(legacy.Negotiation.Reason, "uninit_bg") {
		t.Fatalf("legacy profile negotiated %v: %s", legacy.Negotiation.Support, legacy.Negotiation.Reason)
	}
}

func TestWritableMountRefused(t *testing.T) {
	_, err := Mount(fixture(t, "canonical"), Options{Writable: true})
	if KindOf(err) != KindReadOnly {
		t.Fatalf("writable mount: %v", err)
	}
}
