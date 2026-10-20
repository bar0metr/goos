//go:build !goos_userspace

package arena

// windowTesting lets host tests stop a thread inside the fast-path window.
const windowTesting = true
