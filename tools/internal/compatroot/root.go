// Package compatroot confines generated toolchains to GoOS build output.
package compatroot

import "goos/internal/repobuild"

// Allowed receives a path whose caller has already resolved symlinks. The
// caller also checks that the output itself is a real empty directory.
func Allowed(path string) bool {
	root, err := repobuild.FindRoot("")
	if err != nil {
		return false
	}
	return allowedBuild(path, root)
}

func allowedBuild(path, root string) bool {
	return repobuild.Contains(root, path)
}
