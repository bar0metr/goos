//go:build !goos_userspace

package sys

// window has nothing to map on the host, where no native call succeeds; the
// wrappers exist there only so packages that use them type-check and test.
func window(base uint64, size uint64) []byte { return nil }
