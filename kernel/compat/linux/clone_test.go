package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestProcessCloneFormAcceptsProcessFormsOnly(t *testing.T) {
	cases := []struct {
		number abi.Syscall
		flags  uint64
		stack  uint64
		form   CloneForm
		errno  abi.Errno
	}{
		{abi.SysFork, 0xffff, 0x1000, CloneForm{}, 0},
		{abi.SysVfork, 0, 0, CloneForm{Suspend: true}, 0},
		{abi.SysClone, abi.SignalChild, 0, CloneForm{}, 0},
		{abi.SysClone, abi.SignalChild, 0x7000, CloneForm{Stack: 0x7000}, 0},
		{abi.SysClone, abi.CloneVM | abi.CloneVfork | abi.SignalChild, 0x7000, CloneForm{Suspend: true, Stack: 0x7000}, 0},
		{abi.SysClone, 0, 0, CloneForm{}, abi.EINVAL},
		{abi.SysClone, abi.CloneVM | abi.SignalChild, 0x7000, CloneForm{}, abi.EINVAL},
		{abi.SysClone, abi.CloneVfork | abi.SignalChild, 0, CloneForm{}, abi.EINVAL},
		// CLONE_THREAD|CLONE_SIGHAND|CLONE_VM, the pthread shape.
		{abi.SysClone, 0x10000 | 0x800 | abi.CloneVM, 0x7000, CloneForm{}, abi.EINVAL},
		// CLONE_SETTLS and CLONE_CHILD_SETTID with SIGCHLD.
		{abi.SysClone, 0x80000 | abi.SignalChild, 0, CloneForm{}, abi.EINVAL},
		{abi.SysClone, 0x1000000 | abi.SignalChild, 0, CloneForm{}, abi.EINVAL},
		// CLONE_NEWPID with SIGCHLD.
		{abi.SysClone, 0x20000000 | abi.SignalChild, 0, CloneForm{}, abi.EINVAL},
		{abi.SysClone, 9, 0, CloneForm{}, abi.EINVAL},
		{abi.SysGetpid, 0, 0, CloneForm{}, abi.ENOSYS},
	}
	for _, test := range cases {
		form, errno := ProcessCloneForm(Request{Number: test.number, Arguments: [6]uint64{test.flags, test.stack}})
		if form != test.form || errno != test.errno {
			t.Fatalf("%d flags %#x stack %#x = %+v, errno %d", test.number, test.flags, test.stack, form, errno)
		}
	}
}

func TestProcessCloneFormThread(t *testing.T) {
	const glibc = abi.CloneVM | abi.CloneFS | abi.CloneFiles | abi.CloneSighand | abi.CloneThread | abi.CloneSysvsem |
		abi.CloneSettls | abi.CloneParentSettid | abi.CloneChildCleartid
	form, errno := ProcessCloneForm(Request{Number: abi.SysClone, Arguments: [6]uint64{glibc, 0x7f00, 0x5000, 0x6000, 0x8000}})
	want := CloneForm{Thread: true, Stack: 0x7f00, SetTLS: true, TLS: 0x8000, SetParentTID: true, ParentTID: 0x5000,
		ClearChildTID: true, ChildTID: 0x6000}
	if errno != 0 || form != want {
		t.Fatalf("glibc thread = %+v, errno %d", form, errno)
	}
	musl := abi.CloneVM | abi.CloneFS | abi.CloneFiles | abi.CloneSighand | abi.CloneThread | abi.CloneSysvsem |
		abi.CloneSettls | abi.CloneParentSettid | abi.CloneChildCleartid | abi.CloneDetached
	if form, errno := ProcessCloneForm(Request{Number: abi.SysClone, Arguments: [6]uint64{musl, 0x7f00, 0, 0x6000, 0}}); errno != 0 || !form.ClearChildTID || form.SetChildTID {
		t.Fatalf("musl thread = %+v, errno %d", form, errno)
	}
	for name, flags := range map[string]uint64{
		"no stack":        glibc,
		"exit signal":     glibc | abi.SignalChild,
		"vfork":           glibc | abi.CloneVfork,
		"namespace":       glibc | 0x20000000,
		"missing files":   glibc &^ abi.CloneFiles,
		"missing sighand": glibc &^ abi.CloneSighand,
	} {
		stack := uint64(0x7f00)
		if name == "no stack" {
			stack = 0
		}
		if _, errno := ProcessCloneForm(Request{Number: abi.SysClone, Arguments: [6]uint64{flags, stack}}); errno != abi.EINVAL {
			t.Errorf("%s: errno %d, want EINVAL", name, errno)
		}
	}
}
