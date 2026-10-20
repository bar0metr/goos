package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// goroutinesScenario exercises the native runtime fixture under KVM. It
// does not stand in for the concurrent VFS acceptance scenario.
type goroutinesScenario struct{}

func (goroutinesScenario) Name() string { return "goroutines" }

var goroutineChecks = []struct{ command, marker string }{
	{"goprobe pool", "GOPROBE_POOL_OK"},
	{"goprobe smoke 2", "GOPROBE_SMOKE_OK"},
	{"goprobe smoke 8", "GOPROBE_SMOKE_OK"},
	{"goprobe chan", "GOPROBE_CHAN_OK"},
	{"goprobe sync", "GOPROBE_SYNC_OK"},
	{"goprobe timers", "GOPROBE_TIMERS_OK"},
	{"goprobe fair", "GOPROBE_FAIR_OK"},
	{"goprobe panic", "GOPROBE_PANIC_OK"},
	{"goprobe limit", "GOPROBE_LIMIT_OK"},
	{"goprobe io", "GOPROBE_IO_OK"},
	{"goprobe waitsets", "GOPROBE_WAITSETS_OK"},
	{"goprobe threads", "GOPROBE_THREADS_OK"},
	{"goprobe gc 1", "GOPROBE_GC_OK"},
	{"goprobe gc 8", "GOPROBE_GC_OK"},
}

func goroutineMarker(out, marker string) error {
	matches := 0
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, "GOPROBE_FAIL") || strings.HasPrefix(line, "fatal error:") {
			return fmt.Errorf("fixture reported: %s", line)
		}
		if line == marker {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one %s marker, got %d", marker, matches)
	}
	return nil
}

func (goroutinesScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	var evidence strings.Builder
	for _, check := range goroutineChecks {
		out, err := g.Run(ctx, check.command, commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, check.command, err)
		}
		if err := g.Clean(check.command, out); err != nil {
			return "", err
		}
		if err := goroutineMarker(out, check.marker); err != nil {
			return "", fail(ClassScenario, check.command, err)
		}
		evidence.WriteString(out)
		g.pass(check.command)
	}
	// Timings are evidence, not absolute CI thresholds.
	for sample := 0; sample < 3; sample++ {
		out, err := g.Run(ctx, "goprobe bench", commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, "goroutine-bench", err)
		}
		if err := g.Clean("goroutine-bench", out); err != nil {
			return "", err
		}
		if err := goroutineMarker(out, "GOPROBE_BENCH_OK"); err != nil {
			return "", fail(ClassScenario, "goroutine-bench", err)
		}
		fmt.Fprintf(&evidence, "sample=%d\n%s", sample+1, out)
	}
	if s.EvidenceDir != "" {
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "goroutines.txt"), []byte(evidence.String()), 0600); err != nil {
			return "", fail(ClassInfra, "goroutine-evidence", err)
		}
	}
	g.pass("goroutine-bench")
	return ResultPass, nil
}
