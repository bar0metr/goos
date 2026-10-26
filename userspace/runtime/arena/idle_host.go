//go:build !goos_userspace

package arena

// A host build has no native heap: nothing is pending and nothing collects.

// IdlePending reports whether anything was allocated since the last
// collection, or free pages wait to go back to the kernel.
func IdlePending() bool { return false }

// CollectIdle collects if anything was allocated since the last collection,
// then gives every free page back.
func CollectIdle() {}

// IdleEnter marks the calling thread as waiting with nothing to run.
func IdleEnter() {}

// IdleLeave clears the calling thread's idle mark.
func IdleLeave() {}
