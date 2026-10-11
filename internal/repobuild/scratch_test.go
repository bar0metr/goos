package repobuild_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/internal/repobuild"
	"goos/internal/testscratch"
)

func scratchRepository(t *testing.T) string {
	t.Helper()
	root := testscratch.TempDir(t, "goos-scratch-repository-")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNewScratchCreatesConfinedUniqueChildren(t *testing.T) {
	root := scratchRepository(t)
	first, err := repobuild.NewScratch(root, "child-")
	if err != nil {
		t.Fatal(err)
	}
	second, err := repobuild.NewScratch(root, "child-")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("scratch children are not unique")
	}
	parent := filepath.Join(root, "build", "tmp")
	for _, directory := range []string{first, second} {
		if !filepath.IsAbs(directory) || filepath.Dir(directory) != parent || !strings.HasPrefix(filepath.Base(directory), "child-") {
			t.Fatalf("scratch outside build/tmp: %s", directory)
		}
		if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("scratch child is not a real directory: %v", err)
		}
	}
	marker, err := os.ReadFile(filepath.Join(parent, "go.mod"))
	if err != nil || string(marker) != "module goos.build.scratch\n" {
		t.Fatalf("scratch module boundary: %q: %v", marker, err)
	}
}

func TestNewScratchRejectsRedirectedParentsBeforeWrites(t *testing.T) {
	for _, name := range []string{"build", "tmp"} {
		t.Run(name, func(t *testing.T) {
			root := scratchRepository(t)
			target := testscratch.TempDir(t, "goos-scratch-target-")
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
			if _, err := repobuild.NewScratch(root, "child-"); err == nil {
				t.Fatal("redirected scratch accepted")
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("redirected target changed: %v: %v", entries, err)
			}
		})
	}
}

func TestNewScratchValidatesModuleBoundary(t *testing.T) {
	root := scratchRepository(t)
	if _, err := repobuild.NewScratch(root, "child-"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "build", "tmp", "go.mod")
	for _, replacement := range []func() error{
		func() error { return os.WriteFile(marker, []byte("module other\n"), 0644) },
		func() error {
			if err := os.Remove(marker); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(root, "go.mod"), marker)
		},
	} {
		if err := replacement(); err != nil {
			t.Fatal(err)
		}
		if _, err := repobuild.NewScratch(root, "child-"); err == nil {
			t.Fatal("invalid scratch module marker accepted")
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("module goos.build.scratch\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewScratchRequiresCanonicalRoot(t *testing.T) {
	root := scratchRepository(t)
	if _, err := repobuild.NewScratch(root+string(filepath.Separator)+".", "child-"); err == nil {
		t.Fatal("noncanonical root accepted")
	}
	if _, err := repobuild.NewScratch(root, "bad/prefix"); err == nil {
		t.Fatal("path prefix accepted")
	}
}

func TestRemoveScratchUsesScratchParent(t *testing.T) {
	root := scratchRepository(t)
	directory, err := repobuild.NewScratch(root, "child-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := repobuild.RemoveScratch(root, directory); err != nil {
		t.Fatalf("remove absent scratch: %v", err)
	}

	directory, err = repobuild.NewScratch(root, "child-")
	if err != nil {
		t.Fatal(err)
	}
	target := testscratch.TempDir(t, "goos-scratch-target-")
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, directory); err != nil {
		t.Fatal(err)
	}
	if err := repobuild.RemoveScratch(root, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("replaced scratch not removed: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("symlink target removed: %v", err)
	}
}

func TestRemoveScratchPreservesModuleBoundary(t *testing.T) {
	root := scratchRepository(t)
	if _, err := repobuild.NewScratch(root, "child-"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "build", "tmp", "go.mod")
	for _, path := range []string{marker, filepath.Dir(filepath.Dir(marker))} {
		if err := repobuild.RemoveScratch(root, path); err == nil {
			t.Fatalf("non-scratch directory accepted: %s", path)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "module goos.build.scratch\n" {
		t.Fatalf("scratch module boundary changed: %q: %v", data, err)
	}
}
