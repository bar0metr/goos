package ext4

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestExportMutatedImages writes volumes the mutation code produced, with a
// manifest of what the reader sees in each, to the directory named by
// GOOS_EXT4_EXPORT. tools/ext4fixture/check-mutations.sh runs it and
// checks every image with the pinned e2fsck and debugfs.
func TestExportMutatedImages(t *testing.T) {
	out := os.Getenv("GOOS_EXT4_EXPORT")
	if out == "" {
		t.Skip("GOOS_EXT4_EXPORT not set")
	}
	scenarios := map[string]func(t *testing.T) *scratchDevice{
		"random": func(t *testing.T) *scratchDevice {
			device := runRandomOperations(t, 7, 600)
			fs := ok(mountScratch(device, testClock()))
			// The 5 GiB sparse file would make debugfs dump 5 GiB of zeros.
			if err := fs.Unlink(root(t, fs), []byte("sparse")); err != nil {
				t.Fatal(err)
			}
			return device
		},
		"shapes": func(t *testing.T) *scratchDevice {
			fs, device := scratch(t, "canonical")
			if err := fs.Unlink(root(t, fs), []byte("sparse")); err != nil {
				t.Fatal(err)
			}
			deep := ok(fs.Create(root(t, fs), []byte("deep"), ModeRegular|0o644, 0, 0))
			for index := 0; index < 1500; index++ {
				if _, err := fs.WriteFile(ok(fs.Inode(deep.Number)), uint64(index)*8192, []byte(fmt.Sprintf("x%05d", index))); err != nil {
					t.Fatal(err)
				}
			}
			if err := fs.Truncate(resolve(t, fs, "/fragmented"), 50*8192+7); err != nil {
				t.Fatal(err)
			}
			if _, err := fs.WriteFile(resolve(t, fs, "/empty"), 3*4096+5, []byte("into unwritten")); err != nil {
				t.Fatal(err)
			}
			ok(fs.Symlink(root(t, fs), []byte("fast"), []byte("hello.txt"), 0, 0))
			ok(fs.Symlink(root(t, fs), []byte("slow"), []byte(strings.Repeat("s/", 100)), 0, 0))
			dir := ok(fs.Mkdir(root(t, fs), []byte("moved-in"), 0o750, 12, 34))
			if err := fs.Link(dir, []byte("hello-again"), resolve(t, fs, "/hello.txt")); err != nil {
				t.Fatal(err)
			}
			if err := fs.Rename(root(t, fs), []byte("dir"), resolve(t, fs, "/moved-in"), []byte("dir")); err != nil {
				t.Fatal(err)
			}
			if err := fs.Rename(root(t, fs), []byte("digests"), resolve(t, fs, "/moved-in/dir/sub"), []byte("deep.txt")); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 2000; index += 3 {
				if err := fs.Unlink(resolve(t, fs, "/big"), []byte(fmt.Sprintf("file-%d", index))); err != nil {
					t.Fatal(err)
				}
			}
			return device
		},
		"indexed": func(t *testing.T) *scratchDevice {
			fs, device := scratch(t, "hash-tea")
			for index := 0; index < 2500; index++ {
				name := []byte(fmt.Sprintf("added-%d-\xe9\xff", index))
				if _, err := fs.Create(resolve(t, fs, "/names"), name, ModeRegular|0o644, 0, 0); err != nil {
					t.Fatal(err)
				}
			}
			for index := 0; index < 2500; index += 2 {
				if err := fs.Unlink(resolve(t, fs, "/names"), []byte(fmt.Sprintf("added-%d-\xe9\xff", index))); err != nil {
					t.Fatal(err)
				}
			}
			return device
		},
		"full": func(t *testing.T) *scratchDevice {
			fs, device := scratch(t, "canonical")
			if err := fs.Unlink(root(t, fs), []byte("sparse")); err != nil {
				t.Fatal(err)
			}
			for index := 0; ; index++ {
				file, err := fs.Create(root(t, fs), []byte(fmt.Sprintf("fill-%d", index)), ModeRegular|0o644, 0, 0)
				if err != nil {
					wantKind(t, err, KindNoSpace)
					break
				}
				if _, err := fs.WriteFile(file, 0, make([]byte, 3<<20)); err != nil {
					wantKind(t, err, KindNoSpace)
					break
				}
			}
			for index := 0; index < 3; index++ {
				if err := fs.Unlink(root(t, fs), []byte(fmt.Sprintf("fill-%d", index))); err != nil {
					t.Fatal(err)
				}
			}
			return device
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			device := scenario(t)
			checkVolume(t, device)
			if err := os.WriteFile(filepath.Join(out, name+".img"), device.data, 0o644); err != nil {
				t.Fatal(err)
			}
			manifest := volumeManifest(t, mount(t, device))
			if err := os.WriteFile(filepath.Join(out, name+".manifest.tsv"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// volumeManifest lists the tree in the format tools/ext4fixture/populate.sh
// writes, sorted bytewise, without lost+found.
func volumeManifest(t testing.TB, fs *FS) string {
	var lines []string
	var walk func(dir *Inode, path string)
	walk = func(dir *Inode, path string) {
		var entries []DirEntry
		if _, err := fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
			entries = append(entries, *entry)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		for index := range entries {
			entry := &entries[index]
			child := path + "/" + string(entry.Name())
			if child == "./lost+found" {
				continue
			}
			inode := ok(fs.Inode(entry.Inode))
			switch {
			case inode.IsDir():
				lines = append(lines, "dir\t"+child)
				walk(inode, child)
			case inode.IsSymlink():
				lines = append(lines, "symlink\t"+child+"\t"+string(ok(fs.Readlink(inode))))
			default:
				sum := sha256.Sum256(readAll(t, fs, inode))
				lines = append(lines, fmt.Sprintf("file\t%s\t%d\t%s", child, inode.Size, hex.EncodeToString(sum[:])))
			}
		}
	}
	walk(root(t, fs), ".")
	sort.Slice(lines, func(a, b int) bool { return manifestPath(lines[a]) < manifestPath(lines[b]) })
	return strings.Join(lines, "\n") + "\n"
}

func manifestPath(line string) string { return strings.Split(line, "\t")[1] }
