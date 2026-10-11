package shell

import (
	"strings"
	"testing"

	"goos/userspace/tty"
)

const testPathVariable = "PATH=/system/bin:/user/bin"

func storeTestGeometry(t *testing.T, environment *TerminalEnvironment, geometry tty.Geometry) {
	t.Helper()
	if previous, valid := environment.snapshot.Current(); valid {
		geometry.Sequence = previous.Sequence + 1
	} else {
		geometry.Sequence = 1
	}
	message, ok := tty.GeometryStateReply(geometry, 1)
	if !ok {
		t.Fatal("invalid test geometry")
	}
	_, payload, ok := tty.DecodeReply(&message)
	if !ok || !environment.Store(payload) {
		t.Fatal("invalid test reply")
	}
}

func environmentStrings(environment *TerminalEnvironment) []string {
	var entries []string
	for _, entry := range environment.Entries([]byte(testPathVariable)) {
		entries = append(entries, string(entry))
	}
	return entries
}

func TestRemoteGeometryReachesChildrenWithoutDisplayFlag(t *testing.T) {
	var environment TerminalEnvironment
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 1, Rows: 1000, Authoritative: true, Term: []byte("xterm-256color")})
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=1\nLINES=1000\nTERM=xterm-256color\nGOOS_TTY_SEQUENCE=1\nGOOS_TTY_DISPLAY=0\nGOOS_TTY_AUTHORITATIVE=1" {
		t.Fatalf("environment %q", got)
	}
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 120, Rows: 40, Authoritative: true, Term: []byte("vt100")})
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=120\nLINES=40\nTERM=vt100\nGOOS_TTY_SEQUENCE=2\nGOOS_TTY_DISPLAY=0\nGOOS_TTY_AUTHORITATIVE=1" {
		t.Fatalf("resized environment %q", got)
	}
}

func TestSerialFallbackAndMalformedGeometryRetainsNewestEnvironment(t *testing.T) {
	var environment TerminalEnvironment
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 80, Rows: 25, Display: true, Authoritative: true, Term: []byte("vt100")})
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 80, Rows: 25})
	if got := environmentStrings(&environment); len(got) != 1 || got[0] != testPathVariable {
		t.Fatalf("serial environment %q", got)
	}
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 80, Rows: 25, Authoritative: true, Term: []byte("vt100")})
	if environment.Store([]byte{80, 0, 25, 0, tty.GeometryDisplay}) {
		t.Fatal("v3 payload accepted")
	}
	if got := environmentStrings(&environment); len(got) != 7 {
		t.Fatalf("valid snapshot lost after malformed payload %q", got)
	}
}

func TestDisplayFlagAloneDoesNotMakeGeometryAuthoritative(t *testing.T) {
	var environment TerminalEnvironment
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 80, Rows: 25, Display: true, Term: []byte(strings.Repeat("x", tty.MaxTerm))})
	if got := environmentStrings(&environment); len(got) != 2 || got[1] != "TERM="+strings.Repeat("x", tty.MaxTerm) {
		t.Fatalf("environment %q", got)
	}
}

func TestTerminalEnvironmentCopiesPayloadAndResets(t *testing.T) {
	var environment TerminalEnvironment
	message, _ := tty.GeometryStateReply(tty.Geometry{Columns: 65535, Rows: 65535, Authoritative: true, Term: []byte("vt100")}, 1)
	_, payload, _ := tty.DecodeReply(&message)
	if !environment.Store(payload) {
		t.Fatal("valid geometry refused")
	}
	clear(payload)
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=65535\nLINES=65535\nTERM=vt100\nGOOS_TTY_SEQUENCE=0\nGOOS_TTY_DISPLAY=0\nGOOS_TTY_AUTHORITATIVE=1" {
		t.Fatalf("copied environment %q", got)
	}
	environment.Reset()
	if got := environmentStrings(&environment); len(got) != 1 {
		t.Fatalf("reset environment %q", got)
	}
}

func TestTerminalEnvironmentKeepsNotificationNewerThanStartup(t *testing.T) {
	var environment TerminalEnvironment
	store := func(sequence uint64, columns int) bool {
		message, _ := tty.GeometryStateReply(tty.Geometry{Sequence: sequence, Columns: columns, Rows: 40, Authoritative: true}, 1)
		return environment.Store(message.Payload[1:message.PayloadSize])
	}
	if !store(12, 120) || store(11, 80) || store(12, 80) {
		t.Fatal("late startup or duplicate changed environment")
	}
	got := strings.Join(environmentStrings(&environment), "\n")
	if !strings.Contains(got, "COLUMNS=120\n") || !strings.Contains(got, "GOOS_TTY_SEQUENCE=12\n") {
		t.Fatalf("newest environment %q", got)
	}
	if !store(13, 130) {
		t.Fatal("newer resize refused")
	}
}
