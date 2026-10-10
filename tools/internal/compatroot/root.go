// Package compatroot confines generated toolchains to scratch or GoOS build output.
package compatroot

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// Allowed receives a path whose caller has already resolved symlinks. The
// caller also checks that the output itself is a real empty directory.
func Allowed(path string) bool {
	if strings.HasPrefix(path, "/Volumes/Data/tmp/") {
		return true
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	return allowedBuild(path, cwd)
}

func allowedBuild(path, cwd string) bool {
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.HasPrefix(module, []byte("module goos\n")) {
		return false
	}
	build := filepath.Join(root, "build")
	info, err := os.Lstat(build)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return strings.HasPrefix(path, build+string(filepath.Separator))
}
