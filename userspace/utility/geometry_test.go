package utility

import (
	"goos/userspace/startup"
	"goos/userspace/tty"
	"testing"
)

func geometryStartup(t *testing.T, entries ...string) startup.Data {
	t.Helper()
	var storage [2048]byte
	writer := startup.NewWriter(storage[:], 1, len(entries), 0)
	writer.Command([]byte("gfm"))
	writer.CWD([]byte("/"))
	writer.Argument([]byte("gfm"))
	for _, entry := range entries {
		writer.Entry([]byte(entry))
	}
	encoded, valid := writer.Finish()
	if !valid {
		t.Fatal("startup encode")
	}
	data, valid := startup.Decode(encoded)
	if !valid {
		t.Fatal("startup decode")
	}
	return data
}

func TestRemoteGeometryIsAuthoritativeButNotFixedDisplay(t *testing.T) {
	data := geometryStartup(t, "COLUMNS=120", "LINES=40", "TERM=xterm", tty.SequenceVariable+"=42", tty.DisplayVariable+"=0", tty.AuthoritativeVariable+"=1")
	geometry, valid := TerminalGeometry(data)
	if !valid || geometry.Display || !geometry.Authoritative || geometry.Sequence != 42 || geometry.Columns != 120 || geometry.Rows != 40 {
		t.Fatalf("geometry %+v valid=%v", geometry, valid)
	}
	if _, _, display := TerminalSize(data); display {
		t.Fatal("remote geometry froze framebuffer layout")
	}
}

func TestLocalFramebufferAndSerialFallbackRemainDistinct(t *testing.T) {
	columns, rows, display := TerminalSize(geometryStartup(t, "COLUMNS=106", "LINES=33"))
	if !display || columns != 106 || rows != 33 {
		t.Fatal("legacy framebuffer changed")
	}
	if _, valid := TerminalGeometry(geometryStartup(t, "TERM=vt100")); valid {
		t.Fatal("serial fallback became authoritative")
	}
	if _, valid := TerminalGeometry(geometryStartup(t, "COLUMNS=1001", "LINES=40", tty.DisplayVariable+"=0")); valid {
		t.Fatal("unbounded remote geometry accepted")
	}
	for _, bad := range []string{"", "-1", "18446744073709551616", "1x"} {
		if _, valid := TerminalGeometry(geometryStartup(t, "COLUMNS=80", "LINES=24", tty.SequenceVariable+"="+bad)); valid {
			t.Fatalf("sequence %q accepted", bad)
		}
	}
}
