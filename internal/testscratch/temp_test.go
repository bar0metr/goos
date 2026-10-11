package testscratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/internal/repobuild"
)

func TestTempDirIsUniqueBuildChildAndCleansUp(t *testing.T) {
	root, err := repobuild.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	var first, second string
	t.Run("allocate", func(t *testing.T) {
		first, second = TempDir(t, "goos-scratch-"), TempDir(t, "goos-scratch-")
		for _, path := range []string{first, second} {
			if !filepath.IsAbs(path) || !strings.HasPrefix(path, filepath.Join(root, "build", "tmp")+string(filepath.Separator)) || !repobuild.Contains(root, path) {
				t.Fatalf("scratch outside build/tmp: %s", path)
			}
		}
		if first == second {
			t.Fatal("scratch children are not unique")
		}
	})
	for _, path := range []string{first, second} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("scratch not removed: %s: %v", path, err)
		}
	}
}

func TestScratchRejectsRedirectedParents(t *testing.T) {
	for _, name := range []string{"build", "tmp"} {
		t.Run(name, func(t *testing.T) {
			root := TempDir(t, "goos-scratch-root-")
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos\n"), 0644); err != nil {
				t.Fatal(err)
			}
			target := TempDir(t, "goos-scratch-target-")
			link := filepath.Join(root, "build")
			if name == "tmp" {
				if err := os.Mkdir(link, 0755); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, "tmp")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err := newAt(root, "child-"); err == nil {
				t.Fatal("redirected scratch accepted")
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("redirected target changed: %v: %v", entries, err)
			}
		})
	}
}

func TestScratchModuleBoundary(t *testing.T) {
	root := TempDir(t, "goos-scratch-module-")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos\n"), 0644); err != nil {
		t.Fatal(err)
	}
	directory, err := newAt(root, "child-")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "build", "tmp", "go.mod")
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "module goos.build.scratch\n" {
		t.Fatalf("scratch module boundary: %q: %v", data, err)
	}
	if found, err := repobuild.FindRoot(directory); err != nil || found != root {
		t.Fatalf("discovery through scratch module = %s: %v", found, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "go.mod"), marker); err != nil {
		t.Fatal(err)
	}
	if _, err := newAt(root, "linked-"); err == nil {
		t.Fatal("symlink module boundary accepted")
	}
}
