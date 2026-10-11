package compatroot

import (
	"os"
	"path/filepath"
	"testing"

	"goos/internal/testscratch"
)

func TestBuildRootsStayWithinGoOSOutput(t *testing.T) {
	root, err := filepath.EvalSymlinks(testscratch.TempDir(t, "goos-compatroot-"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos\n\ngo 1.25.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "build"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{filepath.Join(root, "build", "sshcompat", "go"), filepath.Join(root, "build", "stage")} {
		if !allowedBuild(output, root) {
			t.Fatal("build root rejected", output)
		}
	}
	for _, output := range []string{root, filepath.Join(root, "build"), filepath.Join(root, "build-other", "stage"), filepath.Join(root, "tools", "stage"), "/usr/local/go/stage", "/opt/tinygo/stage"} {
		if allowedBuild(output, root) {
			t.Fatal("outside build accepted", output)
		}
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos-other\n"), 0644)
	if allowedBuild(filepath.Join(root, "build", "stage"), root) {
		t.Fatal("foreign module accepted")
	}
}

func TestBuildSymlinkRejected(t *testing.T) {
	root, err := filepath.EvalSymlinks(testscratch.TempDir(t, "goos-compatroot-"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module goos\n"), 0644)
	outside := testscratch.TempDir(t, "goos-compatroot-")
	if err := os.Symlink(outside, filepath.Join(root, "build")); err != nil {
		t.Fatal(err)
	}
	if allowedBuild(filepath.Join(root, "build", "stage"), root) || allowedBuild(filepath.Join(outside, "stage"), root) {
		t.Fatal("redirected build root accepted")
	}
}

func TestAllowedFromPackageDirectory(t *testing.T) {
	output := testscratch.TempDir(t, "goos-compatroot-cwd-")
	if !Allowed(output) {
		t.Fatal("repository build child rejected from package cwd")
	}
	if Allowed(filepath.Join(output, "..", "..", "..", "outside-build")) {
		t.Fatal("outside build accepted from package cwd")
	}
}
