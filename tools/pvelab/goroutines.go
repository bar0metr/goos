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

// goroutineChecks lists each fixture command, its completion marker, the
// measurement records, by name, it must also print once, and an optional
// check of its output.
var goroutineChecks = []struct {
	command, marker string
	records         []string
	verify          func(string) error
}{
	{"goprobe pool", "GOPROBE_POOL_OK", nil, nil},
	{"goprobe smoke 2", "GOPROBE_SMOKE_OK", nil, nil},
	{"goprobe smoke 8", "GOPROBE_SMOKE_OK", nil, nil},
	{"goprobe chan", "GOPROBE_CHAN_OK", nil, nil},
	{"goprobe sync", "GOPROBE_SYNC_OK", nil, nil},
	{"goprobe timers", "GOPROBE_TIMERS_OK", nil, nil},
	{"goprobe fair", "GOPROBE_FAIR_OK", nil, nil},
	{"goprobe panic", "GOPROBE_PANIC_OK", nil, nil},
	{"goprobe limit", "GOPROBE_LIMIT_OK", nil, nil},
	{"goprobe io", "GOPROBE_IO_OK", nil, nil},
	{"goprobe waitsets", "GOPROBE_WAITSETS_OK", []string{"GOPROBE_WAITSETS_JOIN"}, nil},
	{"goprobe logwait", "GOPROBE_LOGWAIT_OK", []string{"GOPROBE_LOGWAIT"}, nil},
	{"goprobe threads", "GOPROBE_THREADS_OK", nil, nil},
	{"goprobe gc 1", "GOPROBE_GC_OK", nil, nil},
	{"goprobe gc 8", "GOPROBE_GC_OK", nil, nil},
	{"goprobe gcstress 1", "GOPROBE_GCSTRESS_OK", []string{"GOPROBE_GCSTRESS"}, nil},
	{"goprobe gcstress 8", "GOPROBE_GCSTRESS_OK", []string{"GOPROBE_GCSTRESS"}, nil},
	{"goprobe swaphold", "GOPROBE_SWAPHOLD_OK", nil, swapHoldRecords},
}

// swapHoldRecords checks the instruction-hold records: one per region, each
// with every expected boundary held and no list lost. Under KVM it shows the
// processor's execute breakpoints and single steps reach each boundary.
func swapHoldRecords(out string) error {
	seen := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if !strings.HasPrefix(line, "GOPROBE_SWAPHOLD region=") {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(line)[1:] {
			key, value, _ := strings.Cut(field, "=")
			fields[key] = value
		}
		region := fields["region"]
		if region != "swap" && region != "preempt" {
			return fmt.Errorf("unknown swaphold region in %q", line)
		}
		seen[region]++
		if fields["expected"] == "" || fields["distinct_rips"] != fields["expected"] {
			return fmt.Errorf("swaphold %s held %s of %s boundaries", region, fields["distinct_rips"], fields["expected"])
		}
		if fields["lost"] != "0" || fields["collections"] == "" || fields["collections"] == "0" {
			return fmt.Errorf("swaphold %s: lost=%s collections=%s", region, fields["lost"], fields["collections"])
		}
	}
	if seen["swap"] != 1 || seen["preempt"] != 1 {
		return fmt.Errorf("expected one swap and one preempt swaphold record, got %d and %d", seen["swap"], seen["preempt"])
	}
	return nil
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

// goroutineRecord checks that output holds exactly one record named name,
// with its fields.
func goroutineRecord(out, name string) error {
	matches := 0
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, name+" ") {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one %s record, got %d", name, matches)
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
		for _, name := range check.records {
			if err := goroutineRecord(out, name); err != nil {
				return "", fail(ClassScenario, check.command, err)
			}
		}
		if check.verify != nil {
			if err := check.verify(out); err != nil {
				return "", fail(ClassScenario, check.command, err)
			}
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
