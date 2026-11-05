package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// linuxStreamStressScenario runs the Linux loopback stream again and again
// (netprobe stress-stream) with a watchdog on every transfer, then one
// long transfer that carries the native stream offsets past 2^32. A
// transfer that stops in time leaves the VM up long enough for the shell to
// take a snapshot: the kernel's diag dump, which shows the family's rings,
// both sides' slots and linuxsockd's state of each socket on a DIAG build,
// and netd's endpoints through net endpoints.
type linuxStreamStressScenario struct{}

func (linuxStreamStressScenario) Name() string { return "linux-stream-stress" }

func (linuxStreamStressScenario) Run(ctx context.Context, s *Session) (Result, error) {
	o := s.Bench
	if o == nil || o.StressCount < 1 || o.StreamMiB < 1 || o.StressTimeout <= 0 {
		return "", failf(ClassConfig, "stress", "the stream stress needs --stress-count, --bench-stream-mib and --stress-timeout")
	}
	if _, err := benchPrepare(ctx, s); err != nil {
		return "", err
	}
	g := s.Guest
	command := fmt.Sprintf("/system/linux/netprobe stress-stream %d %d &", o.StreamMiB, o.StressCount)
	if _, err := g.Run(ctx, command, commandTimeout); err != nil {
		return "", fail(ClassScenario, "stress", err)
	}
	for iteration := 1; iteration <= o.StressCount; iteration++ {
		if err := stressStep(ctx, s, fmt.Sprintf("stress %d ok", iteration), fmt.Sprintf("stress %d failed", iteration), iteration); err != nil {
			return "", err
		}
		if iteration%100 == 0 {
			s.logf("stream stress: %d of %d", iteration, o.StressCount)
		}
	}
	if err := stressStep(ctx, s, "] done", "", o.StressCount); err != nil {
		return "", err
	}
	g.pass("stream-stress")
	if o.StressLongMiB > 0 {
		if _, err := g.Run(ctx, fmt.Sprintf("/system/linux/netprobe long-stream %d &", o.StressLongMiB), commandTimeout); err != nil {
			return "", fail(ClassScenario, "long-stream", err)
		}
		for mark := 256; mark <= o.StressLongMiB; mark += 256 {
			if err := stressStep(ctx, s, fmt.Sprintf("long MiB=%d\r", mark), "long failed", mark); err != nil {
				return "", err
			}
		}
		if err := stressStep(ctx, s, "long ok bytes=", "long failed", o.StressLongMiB); err != nil {
			return "", err
		}
		g.pass("long-stream")
	}
	return ResultPass, nil
}

// stressStep waits for want within the watchdog. A failure line, or no
// progress in time, takes the snapshot and fails the scenario.
func stressStep(ctx context.Context, s *Session, want, failure string, step int) error {
	g := s.Guest
	waitCtx, cancel := context.WithTimeout(ctx, s.Bench.StressTimeout)
	defer cancel()
	needles := [][]byte{[]byte(want), []byte(kernelPanic)}
	if failure != "" {
		needles = append(needles, []byte(failure))
	}
	which, out, err := g.con.ReadUntilAny(waitCtx, needles...)
	switch {
	case err != nil:
		snapshot := stressSnapshot(ctx, s)
		return failf(ClassScenario, "stream-stress", "no progress at step %d (waiting for %q) within %s; snapshot in %s:\n%s",
			step, strings.TrimSpace(want), s.Bench.StressTimeout, snapshot, tailText(g.con))
	case which == 1:
		return failf(ClassBoot, "stream-stress", "kernel panic at step %d:\n%s", step, out)
	case which == 2:
		snapshot := stressSnapshot(ctx, s)
		return failf(ClassScenario, "stream-stress", "step %d failed; snapshot in %s:\n%s", step, snapshot, out)
	}
	return nil
}

// stressSnapshot takes the kernel's diag dump and netd's endpoint dump
// while the stuck transfer still holds its state, and saves them.
func stressSnapshot(ctx context.Context, s *Session) string {
	var parts []string
	for _, command := range []string{"diag", "net endpoints", "services"} {
		out, err := s.Guest.Run(ctx, command, commandTimeout)
		if err != nil {
			out = fmt.Sprintf("%s: %v", command, err)
		}
		parts = append(parts, "goos> "+command, out)
	}
	text := strings.Join(parts, "\n") + "\n"
	if s.EvidenceDir == "" {
		return "(no evidence directory)"
	}
	path := filepath.Join(s.EvidenceDir, "stream-stress-snapshot.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return err.Error()
	}
	return path
}
