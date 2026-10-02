package linux

import "goos/kernel/compat/linux/abi"

// CloneForm is what a process- or thread-creating call asks for.
type CloneForm struct {
	// Suspend holds the parent until the child replaces its image or exits.
	Suspend bool
	// Stack is the child's stack pointer, or zero to keep the parent's.
	Stack uint64
	// Thread creates a task in the caller's process instead of a process.
	Thread bool
	// TLS is the new thread's FS base when SetTLS is set; otherwise the
	// thread starts with the caller's.
	SetTLS bool
	TLS    uint64
	// ParentTID and ChildTID receive the new thread's TID when their Set
	// flags ask. ClearChildTID makes ChildTID the address the kernel zeroes
	// and wakes when the thread ends.
	SetParentTID  bool
	ParentTID     uint64
	SetChildTID   bool
	ClearChildTID bool
	ChildTID      uint64
}

// threadFlags are the flags a thread must carry: it shares memory, the
// filesystem context, descriptors, and signal handlers with its process.
// glibc and musl create every thread with them.
const threadFlags = abi.CloneVM | abi.CloneFS | abi.CloneFiles | abi.CloneSighand | abi.CloneThread

// threadOptions are the flags a thread may add. CLONE_SYSVSEM shares System
// V semaphore undo state, of which GoOS has none; CLONE_DETACHED, which musl
// passes, has been ignored by Linux since 2.6.
const threadOptions = abi.CloneSysvsem | abi.CloneSettls | abi.CloneParentSettid | abi.CloneChildCleartid | abi.CloneChildSettid | abi.CloneDetached

// ProcessCloneForm decodes fork, vfork, and clone. clone accepts three flag
// sets: SIGCHLD alone, a fork; CLONE_VM|CLONE_VFORK|SIGCHLD, a vfork; and
// the thread flags with any of the TLS and TID options, a thread. A forked
// child never shares the parent's memory, whatever CLONE_VM says. Every
// other combination, the namespace flags included, is EINVAL.
func ProcessCloneForm(request Request) (CloneForm, abi.Errno) {
	switch request.Number {
	case abi.SysFork:
		return CloneForm{}, 0
	case abi.SysVfork:
		return CloneForm{Suspend: true}, 0
	case abi.SysClone:
	default:
		return CloneForm{}, abi.ENOSYS
	}
	flags, stack := request.Arguments[0], request.Arguments[1]
	switch flags {
	case abi.SignalChild:
		return CloneForm{Stack: stack}, 0
	case abi.CloneVM | abi.CloneVfork | abi.SignalChild:
		return CloneForm{Suspend: true, Stack: stack}, 0
	}
	// A thread ends without a signal to anyone, and a thread that ran on
	// its creator's stack would corrupt it.
	if flags&threadFlags != threadFlags || flags&^(threadFlags|threadOptions) != 0 || stack == 0 {
		return CloneForm{}, abi.EINVAL
	}
	// x86_64 clone takes parent_tid, child_tid, and tls in that order.
	return CloneForm{
		Thread:        true,
		Stack:         stack,
		SetTLS:        flags&abi.CloneSettls != 0,
		TLS:           request.Arguments[4],
		SetParentTID:  flags&abi.CloneParentSettid != 0,
		ParentTID:     request.Arguments[2],
		SetChildTID:   flags&abi.CloneChildSettid != 0,
		ClearChildTID: flags&abi.CloneChildCleartid != 0,
		ChildTID:      request.Arguments[3],
	}, 0
}
