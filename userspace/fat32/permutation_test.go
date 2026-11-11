package fat32

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"goos/kernel/ramfs"
)

func TestUnicodeFixtureCreationPermutations(t *testing.T) {
	fixtures := []struct{ path, content string }{
		{"Документы/Фото с моря.txt", "море"},
		{"日本語/音乐.txt", "音乐"},
		{"emoji-😀.txt", "😀"},
	}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			path, file := imageFile(t, 40<<20)
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			device := openDevice(t, path)
			if !Format(device, Layout{SectorsPerCluster: 1, FATs: 2}) {
				t.Fatal("format failed")
			}
			fs, root := mounted(t, device)
			for _, index := range order {
				fixture := fixtures[index]
				parts := strings.Split(fixture.path, "/")
				dir := root
				if len(parts) == 2 {
					dir = mkdir(t, fs, root, parts[0])
				}
				writeFile(t, fs, dir, parts[len(parts)-1], fixture.content)
			}
			if !device.Flush() {
				t.Fatal("flush failed")
			}
			fs, root = mounted(t, device)
			for _, fixture := range fixtures {
				node := root
				for _, part := range strings.Split(fixture.path, "/") {
					node = mustOpen(t, fs, node, part)
				}
				if got := readAll(t, fs, node, len(fixture.content), 512); !bytes.Equal(got, []byte(fixture.content)) {
					t.Fatalf("%s: got %q", fixture.path, got)
				}
			}
			var issues issueList
			report := Check(device, &issues)
			if report.Status != ramfs.StatusOK || report.Issues != 0 {
				t.Fatalf("check: %+v, issues: %+v", report, issues)
			}
			if report.Files != 3 || report.Directories != 3 || report.UsedClusters+report.FreeClusters != fs.clusters {
				t.Fatalf("unexpected allocation report: %+v", report)
			}
			var info [512]byte
			if !device.ReadAt(fs.fsInfo, info[:]) {
				t.Fatal("read FSInfo failed")
			}
			free, hint := get32(info[fsInfoFreeCount:]), get32(info[fsInfoNextFree:])
			if free != report.FreeClusters || !fs.valid(hint) {
				t.Fatalf("FSInfo: free=%d, hint=%d, report=%+v", free, hint, report)
			}
			if value, status := fs.fat(hint); status != ramfs.StatusOK || value != 0 {
				t.Fatalf("FSInfo hint %d is not free: %#x (%v)", hint, value, status)
			}
			if mismatch, status := fs.CompareFATs(); status != ramfs.StatusOK || mismatch.Entries != 0 {
				t.Fatalf("FAT copies: %+v (%v)", mismatch, status)
			}
			if !device.Flush() {
				t.Fatal("sync before host check failed")
			}
			if err := device.file.Close(); err != nil {
				t.Fatalf("close before host check: %v", err)
			}
			if tool, err := exec.LookPath("fsck_msdos"); err == nil {
				image, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				_, hashErr := io.Copy(hash, image)
				closeErr := image.Close()
				if hashErr != nil || closeErr != nil {
					t.Fatalf("hash image: read=%v close=%v", hashErr, closeErr)
				}
				digest := fmt.Sprintf("%x", hash.Sum(nil))
				t.Logf("fsck image=%s sha256=%s order=%v", path, digest, order)
				output, err := exec.Command(tool, "-n", path).CombinedOutput()
				if err != nil {
					t.Fatalf("fsck_msdos image=%s sha256=%s order=%v: %v\n%s", path, digest, order, err, output)
				}
			} else {
				t.Log("fsck_msdos unavailable")
			}
		})
	}
}
