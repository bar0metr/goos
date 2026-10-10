package main

import (
	"goos/userspace/remoteapi"
	"goos/userspace/sshserver"
	"testing"
)

func TestWindowChangePreservesPTYTerm(t *testing.T) {
	current, ok := remoteapi.MakeGeometry(80, 24, []byte("xterm-256color"))
	if !ok {
		t.Fatal("fixture")
	}
	next, ok := geometryForEvent(current, sshserver.Event{Kind: sshserver.Resize, Columns: 100, Rows: 30})
	if !ok || next.Columns != 100 || next.Rows != 30 || string(next.Term[:next.TermSize]) != "xterm-256color" {
		t.Fatal("resize lost geometry or TERM", next)
	}
	if current.Columns != 80 || current.Rows != 24 {
		t.Fatal("mutated prior geometry")
	}
}

func TestPTYGeometryUsesRequestTerm(t *testing.T) {
	event := sshserver.Event{Kind: sshserver.PTY, Columns: 80, Rows: 24, TermLen: 5}
	copy(event.Term[:], "vt100")
	next, ok := geometryForEvent(remoteapi.Geometry{}, event)
	if !ok || string(next.Term[:next.TermSize]) != "vt100" {
		t.Fatal("PTY TERM ignored")
	}
	for _, size := range []uint8{0, 65, 255} {
		event.TermLen = size
		if _, ok := geometryForEvent(remoteapi.Geometry{}, event); ok {
			t.Fatal("invalid TERM size", size)
		}
	}
}

func TestWindowChangeRejectsInvalidGeometry(t *testing.T) {
	current, _ := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	for _, event := range []sshserver.Event{
		{Kind: sshserver.Resize, Columns: 0, Rows: 24},
		{Kind: sshserver.Resize, Columns: 80, Rows: 1001},
		{Kind: sshserver.Data, Columns: 80, Rows: 24},
	} {
		if _, ok := geometryForEvent(current, event); ok {
			t.Fatal("invalid event accepted", event.Kind)
		}
	}
	current.TermSize = 255
	if _, ok := geometryForEvent(current, sshserver.Event{Kind: sshserver.Resize, Columns: 80, Rows: 24}); ok {
		t.Fatal("invalid current TERM accepted")
	}
}
