package main

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openHostPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(int(master.Fd()), request, 0); err != nil {
			master.Close()
			return nil, nil, err
		}
	}
	var name [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		master.Close()
		return nil, nil, errno
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		master.Close()
		return nil, nil, unix.EINVAL
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func rawHostPTY(slave *os.File) error {
	state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		return err
	}
	makeHostRaw(state)
	return unix.IoctlSetTermios(int(slave.Fd()), unix.TIOCSETA, state)
}
