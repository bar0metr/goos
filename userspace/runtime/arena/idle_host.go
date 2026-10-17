//go:build !goos_userspace

package arena

// A host build has no native heap: nothing is pending and nothing collects.

// IdlePending reports whether anything was allocated since the last
// collection.
func IdlePending() bool { return false }

// CollectIdle collects if anything was allocated since the last collection.
func CollectIdle() {}
