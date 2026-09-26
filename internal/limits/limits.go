// Package limits holds the native naming limits every layer shares: the
// shell, the exec protocol, the startup ABI, the VFS, and the filesystems
// (ADR 0110).
package limits

const (
	// NameMax is the longest name of one path component, in bytes.
	NameMax = 255
	// PathMax is the longest absolute path, in bytes.
	PathMax = 4096
)
