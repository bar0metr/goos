//go:build !goos_validation

package validation

// Enabled reports whether this is a validation build. Validation builds print
// the *_OK markers the boot tests assert and run fault-injection and self-test
// scenarios; a normal build does neither.
const Enabled = false
