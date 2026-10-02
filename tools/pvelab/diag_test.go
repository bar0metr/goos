package main

import (
	"reflect"
	"testing"
)

// A line as kernel/diagnostics.go prints it.
const recordedDiag = "DIAG linux syscalls=4182 unsupported=7 last-unsupported=302 recent-unsupported=302,302,99,63 refused=63,99,302 bad-pointers=1 last-bad-pointer=16 vfs-requests=211 vfs-failures=0 vfs-stale=0"

func TestParseDiagLinux(t *testing.T) {
	d, err := ParseDiagLinux(recordedDiag)
	if err != nil {
		t.Fatal(err)
	}
	if d.Syscalls != 4182 || d.Unsupported != 7 || d.LastUnsupported != 302 || d.BadPointers != 1 || d.LastBadPointer != 16 || d.VFSRequests != 211 {
		t.Errorf("numbers %+v", d)
	}
	if !reflect.DeepEqual(d.RecentUnsupported, []uint64{302, 302, 99, 63}) || !reflect.DeepEqual(d.Refused, []uint64{63, 99, 302}) {
		t.Errorf("lists %v %v", d.RecentUnsupported, d.Refused)
	}
}

func TestParseDiagLinuxOrderEmptyAndUnknown(t *testing.T) {
	d, err := ParseDiagLinux("DIAG linux refused= future-field=abc unsupported=0 recent-unsupported= syscalls=0x10")
	if err != nil {
		t.Fatal(err)
	}
	if d.Syscalls != 16 || len(d.Refused) != 0 || len(d.RecentUnsupported) != 0 || d.Extra["future-field"] != "abc" {
		t.Errorf("%+v", d)
	}
}

func TestParseDiagLinuxRejects(t *testing.T) {
	for _, line := range []string{
		"DIAG waits ipc=1",
		"DIAG linuxx syscalls=1",
		"DIAG linux syscalls=many",
		"DIAG linux refused=1,,2",
		"DIAG linux noequals",
	} {
		if _, err := ParseDiagLinux(line); err == nil {
			t.Errorf("%q accepted", line)
		}
	}
}

func TestFindDiagLinux(t *testing.T) {
	text := "DIAG waits ipc=0\r\nDIAG linux syscalls=1\r\nDIAG linux syscalls=2\r\nDIAG end\r\n"
	line, ok := FindDiagLinux(text)
	if !ok || line != "DIAG linux syscalls=2" {
		t.Errorf("%q %v", line, ok)
	}
	if _, ok := FindDiagLinux("nothing"); ok {
		t.Error("found a line in nothing")
	}
}
