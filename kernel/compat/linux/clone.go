package linux

import "goos/kernel/compat/linux/abi"

// CloneForm is what a process-creating call asks for.
type CloneForm struct {
	// Suspend holds the parent until the child replaces its image or exits.
	Suspend bool
	// Stack is the child's stack pointer, or zero to keep the parent's.
	Stack uint64
}

// ProcessCloneForm decodes fork, vfork, and clone. clone accepts two flag
// sets: SIGCHLD alone, a fork, and CLONE_VM|CLONE_VFORK|SIGCHLD, a vfork. The
// child never shares the parent's memory, whatever CLONE_VM says. Every other
// flag, including the thread, TLS, TID, and namespace flags, is EINVAL.
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
	return CloneForm{}, abi.EINVAL
}
