package repobuild_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/internal/repobuild"
	"goos/internal/testscratch"
)

func repository(t *testing.T, module string) string {
	t.Helper()
	root := testscratch.TempDir(t, "goos-repobuild-")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "build"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFindRootFromPackageDirectory(t *testing.T) {
	root := repository(t, "// license\n\nmodule goos // project\n\ngo 1.25.0\n")
	packageDir := filepath.Join(root, "tools", "sshcompat")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{root, packageDir} {
		found, err := repobuild.FindRoot(start)
		if err != nil || found != root {
			t.Fatalf("root from %s = %s: %v", start, found, err)
		}
	}
}

func TestBuildDirRequiresExactModuleDeclaration(t *testing.T) {
	for _, text := range []string{
		"module goos\n", "module \"goos\"\n", "/* license */\nmodule goos\r\n",
	} {
		root := repository(t, text)
		if _, err := repobuild.BuildDir(root); err != nil {
			t.Errorf("valid module %q: %v", text, err)
		}
	}
	for _, text := range []string{
		"module goos-other\n", "module goos/child\n", "// module goos\n", "/*\nmodule goos\n*/\n",
		"module goos extra\n", "module goos\nmodule goos\n", "module `goos`\n",
	} {
		root := repository(t, text)
		if _, err := repobuild.BuildDir(root); err == nil {
			t.Errorf("invalid module accepted: %q", text)
		}
	}
}

func TestFindRootRejectsUnsafeModuleMarker(t *testing.T) {
	root := repository(t, "module goos\n")
	marker := filepath.Join(root, "go.mod")
	saved := filepath.Join(root, "module-saved")
	if err := os.Rename(marker, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := repobuild.FindRoot(root); err == nil {
		t.Fatal("symlink module marker accepted")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := repobuild.FindRoot(root); err == nil {
		t.Fatal("directory module marker accepted")
	}
}

func TestFindRootBoundsParentSearch(t *testing.T) {
	root := testscratch.TempDir(t, "goos-root-depth-")
	deep := root
	for depth := 0; depth < 72; depth++ {
		deep = filepath.Join(deep, "child")
	}
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := repobuild.FindRoot(deep); err == nil || !strings.Contains(err.Error(), "no GoOS module root") {
		t.Fatalf("unbounded search: %v", err)
	}
	if _, err := repobuild.FindRoot(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing start accepted")
	}
}

func TestContainsRejectsEscapesAndLinkedDirectories(t *testing.T) {
	root := repository(t, "module goos\n")
	build := filepath.Join(root, "build")
	if !repobuild.Contains(root, filepath.Join(build, "tmp", "future")) {
		t.Fatal("missing build child rejected")
	}
	for _, path := range []string{root, build, filepath.Join(root, "build-other", "child"), filepath.Join(root, "tools", "child"), build + "/../outside", "build/tmp/relative"} {
		if repobuild.Contains(root, path) {
			t.Errorf("outside or noncanonical path accepted: %s", path)
		}
	}
	outside := testscratch.TempDir(t, "goos-root-target-")
	link := filepath.Join(build, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if repobuild.Contains(root, filepath.Join(link, "child")) {
		t.Fatal("linked build component accepted")
	}
	if err := os.Rename(build, filepath.Join(root, "old-build")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, build); err != nil {
		t.Fatal(err)
	}
	if repobuild.Contains(root, filepath.Join(build, "child")) {
		t.Fatal("symlinked build accepted")
	}
	if err := os.Remove(build); err != nil {
		t.Fatal(err)
	}
	if repobuild.Contains(root, filepath.Join(build, "child")) {
		t.Fatal("missing build accepted")
	}
}
