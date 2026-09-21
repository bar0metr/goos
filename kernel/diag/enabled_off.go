//go:build !goos_diag

package diag

// Enabled reports whether this build records and dumps diagnostics. Without
// the goos_diag tag every recording call compiles away.
const Enabled = false
