package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

// This opens only a host PTY; it starts no SSH client or VM.
func TestHostPTYGeometry(t *testing.T) {
	master, slave, err := openHostPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := rawHostPTY(slave); err != nil {
		t.Fatal(err)
	}
	for _, size := range []unix.Winsize{{Row: 24, Col: 80}, {Row: 40, Col: 120}, {Row: 44, Col: 132}} {
		if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &size); err != nil {
			t.Fatal(err)
		}
		observed, err := unix.IoctlGetWinsize(int(slave.Fd()), unix.TIOCGWINSZ)
		if err != nil || *observed != size {
			t.Fatalf("geometry %+v, want %+v: %v", observed, size, err)
		}
	}
}
