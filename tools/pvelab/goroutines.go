package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	{"goprobe regroots", "GOPROBE_REGROOTS_OK", nil},
}

// goroutineVerifiers check the records of the commands whose output a
// single named record does not cover.
var goroutineVerifiers = map[string]func(string) error{
	"goprobe regroots": regRootsRecords,
}

// goroutineTimeouts replaces commandTimeout for commands that run longer:
// goprobe regroots may spend its entry budget on each of 31 registers.
var goroutineTimeouts = map[string]time.Duration{
	"goprobe regroots": 300 * time.Second,
}

// regClasses maps each register class goprobe regroots checks to its
// register count; regCollections is how many collections each register's
// hold spans.
var regClasses = map[string]uint64{"xmm": 16, "gpr": 15}

const regCollections = 4

// regRootsRecords checks goprobe regroots output: one record per register
// class and situation, each covering every register of the class with no
// list lost, and each goroutine one covering the running phase, with at
// least one hold inside the preemption entry, and the switched-out phase.
func regRootsRecords(out string) error {
	seen := map[string]map[string]uint64{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, "GOPROBE_REGROOTS_LOST ") || strings.HasPrefix(line, "GOPROBE_REGROOTS_KEPT ") {
			return fmt.Errorf("fixture reported: %s", line)
		}
		rest, found := strings.CutPrefix(line, "GOPROBE_REGROOTS ")
		if !found {
			continue
		}
		fields := map[string]uint64{}
		class, situation := "", ""
		for _, field := range strings.Fields(rest) {
			name, value, found := strings.Cut(field, "=")
			if !found {
				return fmt.Errorf("malformed field %q in %q", field, line)
			}
			switch name {
			case "class":
				class = value
				continue
			case "situation":
				situation = value
				continue
			}
			number, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("malformed field %q in %q", field, line)
			}
			fields[name] = number
		}
		key := class + "/" + situation
		if _, dup := seen[key]; dup {
			return fmt.Errorf("two %s records", key)
		}
		seen[key] = fields
	}
	if len(seen) != 2*len(regClasses) {
		return fmt.Errorf("expected %d records, got %d", 2*len(regClasses), len(seen))
	}
	for class, registers := range regClasses {
		least := registers * regCollections
		for _, situation := range []string{"thread", "goroutine"} {
			key := class + "/" + situation
			fields, found := seen[key]
			if !found {
				return fmt.Errorf("no %s record", key)
			}
			if fields["regs"] != registers || fields["lost"] != 0 || fields["collections"] < least {
				return fmt.Errorf("%s record: regs=%d lost=%d collections=%d", key, fields["regs"], fields["lost"], fields["collections"])
			}
			if fields["controls"] == 0 || fields["reclaimed"] != fields["controls"] {
				return fmt.Errorf("%s record: controls=%d reclaimed=%d", key, fields["controls"], fields["reclaimed"])
			}
		}
		g := seen[class+"/goroutine"]
		if g["running"] < least || g["switched"] < least || g["switched_preemptions"] == 0 || g["entry_regs"] == 0 {
			return fmt.Errorf("%s goroutine record: running=%d switched=%d switched_preemptions=%d entry_regs=%d",
				class, g["running"], g["switched"], g["switched_preemptions"], g["entry_regs"])
		}
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
		timeout := commandTimeout
		if longer, found := goroutineTimeouts[check.command]; found {
			timeout = longer
		}
		out, err := g.Run(ctx, check.command, timeout)
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
