package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// goroutinesScenario exercises the native runtime fixture under KVM. It
// does not stand in for the concurrent VFS acceptance scenario.
type goroutinesScenario struct{}

func (goroutinesScenario) Name() string { return "goroutines" }

// goroutineChecks lists each fixture command, its completion marker, and
// the measurement records, by name, it must also print once.
var goroutineChecks = []struct {
	command, marker string
	records         []string
}{
	{"goprobe pool", "GOPROBE_POOL_OK", nil},
	{"goprobe smoke 2", "GOPROBE_SMOKE_OK", nil},
	{"goprobe smoke 8", "GOPROBE_SMOKE_OK", nil},
	{"goprobe chan", "GOPROBE_CHAN_OK", nil},
	{"goprobe sync", "GOPROBE_SYNC_OK", nil},
	{"goprobe timers", "GOPROBE_TIMERS_OK", nil},
	{"goprobe fair", "GOPROBE_FAIR_OK", nil},
	{"goprobe panic", "GOPROBE_PANIC_OK", nil},
	{"goprobe limit", "GOPROBE_LIMIT_OK", nil},
	{"goprobe io", "GOPROBE_IO_OK", nil},
	{"goprobe waitsets", "GOPROBE_WAITSETS_OK", []string{"GOPROBE_WAITSETS_JOIN"}},
	{"goprobe logwait", "GOPROBE_LOGWAIT_OK", []string{"GOPROBE_LOGWAIT"}},
	{"goprobe threads", "GOPROBE_THREADS_OK", nil},
	{"goprobe gc 1", "GOPROBE_GC_OK", nil},
	{"goprobe gc 8", "GOPROBE_GC_OK", nil},
	{"goprobe gcstress 1", "GOPROBE_GCSTRESS_OK", []string{"GOPROBE_GCSTRESS"}},
	{"goprobe gcstress 8", "GOPROBE_GCSTRESS_OK", []string{"GOPROBE_GCSTRESS"}},
	{"goprobe xmmroots", "GOPROBE_XMMROOTS_OK", nil},
}

// goroutineVerifiers check the records of the commands whose output a
// single named record does not cover.
var goroutineVerifiers = map[string]func(string) error{
	"goprobe xmmroots": xmmRootsRecords,
}

// xmmRegisters is how many XMM registers goprobe xmmroots checks, and
// xmmCollections how many collections each register's hold spans.
const (
	xmmRegisters   = 16
	xmmCollections = 4
)

// xmmRootsRecords checks goprobe xmmroots output: one record per situation,
// each covering every register with no list lost, and the goroutine one
// covering the running phase, with at least one hold inside the preemption
// entry, and the switched-out phase.
func xmmRootsRecords(out string) error {
	seen := map[string]map[string]uint64{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, "GOPROBE_XMMROOTS_LOST ") || strings.HasPrefix(line, "GOPROBE_XMMROOTS_KEPT ") {
			return fmt.Errorf("fixture reported: %s", line)
		}
		rest, found := strings.CutPrefix(line, "GOPROBE_XMMROOTS ")
		if !found {
			continue
		}
		fields := map[string]uint64{}
		situation := ""
		for _, field := range strings.Fields(rest) {
			name, value, found := strings.Cut(field, "=")
			if !found {
				return fmt.Errorf("malformed field %q in %q", field, line)
			}
			if name == "situation" {
				situation = value
				continue
			}
			number, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("malformed field %q in %q", field, line)
			}
			fields[name] = number
		}
		if _, dup := seen[situation]; dup {
			return fmt.Errorf("two %s records", situation)
		}
		seen[situation] = fields
	}
	if len(seen) != 2 {
		return fmt.Errorf("expected thread and goroutine records, got %d", len(seen))
	}
	least := uint64(xmmRegisters * xmmCollections)
	for _, situation := range []string{"thread", "goroutine"} {
		fields, found := seen[situation]
		if !found {
			return fmt.Errorf("no %s record", situation)
		}
		if fields["regs"] != xmmRegisters || fields["lost"] != 0 || fields["collections"] < least {
			return fmt.Errorf("%s record: regs=%d lost=%d collections=%d", situation, fields["regs"], fields["lost"], fields["collections"])
		}
		if fields["controls"] == 0 || fields["reclaimed"] != fields["controls"] {
			return fmt.Errorf("%s record: controls=%d reclaimed=%d", situation, fields["controls"], fields["reclaimed"])
		}
	}
	if g := seen["goroutine"]; g["running"] < least || g["switched"] < least || g["switched_preemptions"] == 0 || g["entry_regs"] == 0 {
		return fmt.Errorf("goroutine record: running=%d switched=%d switched_preemptions=%d entry_regs=%d",
			g["running"], g["switched"], g["switched_preemptions"], g["entry_regs"])
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
		if verify := goroutineVerifiers[check.command]; verify != nil {
			if err := verify(out); err != nil {
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
