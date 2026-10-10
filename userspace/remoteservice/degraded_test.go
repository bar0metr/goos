package remoteservice

import (
	"strings"
	"testing"
)

func TestDegradedStatusRecoveryAndRollback(t *testing.T) {
	var state State
	state.Reject("bad configuration\ncontrol")
	text := string(state.Status(0, 0))
	if !strings.Contains(text, "service=running listener=closed settings=invalid reason=bad configuration?control") || strings.Contains(text, "fingerprint=") || strings.Contains(text, "bind=") {
		t.Fatal(text)
	}
	state.Reject("identity unavailable")
	if !strings.Contains(string(state.Status(0, 0)), "reason=identity unavailable\n") {
		t.Fatal("reason not replaced")
	}
	policy, host, keys := fixture(t)
	next, err := state.Prepare(policy, host, keys)
	if err != nil || !state.Commit(next) {
		t.Fatal("recovery", err)
	}
	state.Reject("rejected replacement")
	text = string(state.Status(0, 0))
	if strings.Contains(text, "reason=") || !strings.Contains(text, "settings=configured") || !strings.Contains(text, "user=user") || state.Current != next {
		t.Fatal(text)
	}
}

func TestDegradedReasonBound(t *testing.T) {
	var state State
	state.Reject(strings.Repeat("x", 1024))
	if int(state.reasonSize) != len(state.reason) {
		t.Fatal("unbounded reason")
	}
	state.Reject("short")
	if state.reasonSize != 5 || state.reason[5] != 0 {
		t.Fatal("stale reason bytes")
	}
}
