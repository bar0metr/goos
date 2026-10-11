package supervisor

import (
	"goos/userspace/commands"
	"goos/userspace/tty"
	"testing"
)

func TestResizeOwnerBackgroundDenialAndRawExit(t *testing.T) {
	var owner ResizeOwner
	foreground := oneStage(t, "/system/bin/gfm", false)
	if !owner.Start(1, foreground, commands.GrantTerminal) || !owner.Allows(1, false) {
		t.Fatal("foreground denied")
	}
	if owner.Start(2, oneStage(t, "/system/bin/gfm", true), commands.GrantTerminal) || owner.Allows(2, false) {
		t.Fatal("background got resize authority")
	}
	if !owner.Allows(1, false) {
		t.Fatal("background stole foreground")
	}
	owner.Release(2)
	if !owner.Allows(1, false) {
		t.Fatal("unrelated reap released foreground")
	}
	owner.Release(1)
	if owner.Allows(1, false) || owner.Handoff(1) || owner.Restore(1) {
		t.Fatal("same process retained authority after raw exit")
	}
}

func TestResizeOwnerNestedChildRestoresNewestParentSnapshot(t *testing.T) {
	var owner ResizeOwner
	var session, parent, child tty.Snapshot
	size := func(sequence uint64, columns, rows int) tty.Geometry {
		return tty.Geometry{Sequence: sequence, Columns: columns, Rows: rows, Authoritative: true}
	}
	owner.Start(7, oneStage(t, "/system/bin/gfm", false), commands.GrantTerminal)
	session.Apply(size(1, 80, 24))
	parent.Apply(size(1, 80, 24))
	if !owner.Handoff(7) || owner.Allows(7, false) || !owner.Allows(7, true) {
		t.Fatal("parent not suspended")
	}
	child.Apply(size(2, 120, 40))
	if child.Apply(size(1, 80, 24)) {
		t.Fatal("late child startup overwrote resize")
	}
	session.Apply(size(3, 130, 45))
	current, _ := session.Current()
	child.Apply(current)
	if !owner.Restore(7) || !owner.Allows(7, false) || owner.Allows(7, true) {
		t.Fatal("child retained ownership after reap")
	}
	parent.Apply(current)
	current, _ = parent.Current()
	if current.Sequence != 3 || current.Columns != 130 || current.Rows != 45 {
		t.Fatalf("parent restored old geometry %+v", current)
	}
	owner.Handoff(7)
	owner.Release(7)
	if owner.Restore(7) {
		t.Fatal("raw exit restored nested authority")
	}
}

func TestResizeOwnerHandoffRevokesPreviousForeground(t *testing.T) {
	var owner ResizeOwner
	request := oneStage(t, "/system/bin/gfm", false)
	owner.Start(1, request, commands.GrantTerminal)
	owner.Start(2, request, commands.GrantTerminal)
	if owner.Allows(1, false) || owner.Handoff(1) || !owner.Allows(2, false) {
		t.Fatal("old foreground retained resize")
	}
	request.Count = 2
	if owner.Start(3, request, commands.GrantTerminal) {
		t.Fatal("pipeline received resize endpoint")
	}
	if owner.Start(3, oneStage(t, "/system/bin/ls", false), commands.GrantVFS) {
		t.Fatal("non-raw command received resize")
	}
}
