// Package version names the release the kernel and utilities report.
//
// Release is the only place the version is written. The kernel banner,
// uname, the read-only system.build.release setting, `make version`, and the
// boot test's expectation all read it from here, so a release is one edit
// followed by a rebuild.
package version

const (
	Name    = "GoOS"
	Release = "0.1.7"
	Machine = "x86_64"
)
