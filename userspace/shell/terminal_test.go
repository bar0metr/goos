package shell

import (
	"strings"
	"testing"

	"goos/userspace/tty"
)

const testPathVariable = "PATH=/system/bin:/user/bin"

func storeTestGeometry(t *testing.T, environment *TerminalEnvironment, geometry tty.Geometry) {
	t.Helper()
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
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=1\nLINES=1000\nTERM=xterm-256color" {
		t.Fatalf("environment %q", got)
	}
	storeTestGeometry(t, &environment, tty.Geometry{Columns: 120, Rows: 40, Authoritative: true, Term: []byte("vt100")})
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=120\nLINES=40\nTERM=vt100" {
		t.Fatalf("resized environment %q", got)
	}
}

func TestSerialFallbackAndMalformedGeometryClearStaleEnvironment(t *testing.T) {
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
	if got := environmentStrings(&environment); len(got) != 1 {
		t.Fatalf("stale environment %q", got)
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
	if got := strings.Join(environmentStrings(&environment), "\n"); got != testPathVariable+"\nCOLUMNS=65535\nLINES=65535\nTERM=vt100" {
		t.Fatalf("copied environment %q", got)
	}
	environment.Reset()
	if got := environmentStrings(&environment); len(got) != 1 {
		t.Fatalf("reset environment %q", got)
	}
}
