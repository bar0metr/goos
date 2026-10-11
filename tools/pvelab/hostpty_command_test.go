package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHostPTYCommandHasGeometryAndReturnsStatus(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := runHostPTYCommand(ctx, "sh", "-c", "stty size; printf '%s' \"$TERM\" >&2; exit 7")
	if err != nil || out.Status != 7 || strings.TrimSpace(out.Stdout) != "24 80" || out.Stderr != "xterm" {
		t.Fatalf("PTY command %+v: %v", out, err)
	}
}

func TestRemoteAdminPTYProbeUsesTerminalRunner(t *testing.T) {
	for _, allocationFailure := range []bool{false, true} {
		m := newScenarioMock(t)
		calls := 0
		m.s.HostPTYRunner = func(ctx context.Context, name string, args ...string) (commandResult, error) {
			calls++
			if name != "ssh" || args[len(args)-1] != "uname" || !strings.Contains(strings.Join(args, " "), "-tt") {
				t.Fatalf("invalid PTY probe %s %v", name, args)
			}
			if allocationFailure {
				return commandResult{Status: 255, Stderr: "PTY allocation request failed on channel 0\n"}, nil
			}
			return m.run(ctx, name, args...)
		}
		result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
		if calls != 1 {
			t.Fatalf("terminal runner called %d times", calls)
		}
		if allocationFailure {
			if err == nil || !strings.Contains(err.Error(), "PTY + exec was not explicitly rejected") {
				t.Fatalf("PTY allocation failure accepted: %s %v", result, err)
			}
		} else if err != nil || result != ResultPass {
			t.Fatalf("PTY exec probe %s: %v", result, err)
		}
	}
}
