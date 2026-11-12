package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ext4BenchScenario measures an ext4 volume mounted read-write (ADR 0226,
// ADR 0227) with fsbench, the static Linux program the Linux reference runs
// on the same volume layout: names, sequential and random 4 KiB I/O, fsync
// latency, the replace pattern, a sparse file past 4 GiB and 1, 2, 4 and 8
// processes at once, then once on RAMFS and once on the persistent FAT32
// volume for comparison. The disk carries a partition labelled goos-bench that
// tools/ext4fixture/make-bench-volume.sh made; make test-pve-ext4 builds it.
type ext4BenchScenario struct{}

// ext4BenchRounds is how many times fsbench runs; the report keeps every
// round so a reader can take the median.
const ext4BenchRounds = 3

// ext4BenchTimeout bounds one fsbench run.
const ext4BenchTimeout = 10 * time.Minute

func (ext4BenchScenario) Name() string { return "ext4-bench" }

func (ext4BenchScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	listing, err := g.Run(ctx, "ls /system/linux", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "ext4-bench", err)
	}
	if !hasLine(listing, "fsbench") {
		return "", failf(ClassConfig, "ext4-bench", "/system/linux/fsbench is missing")
	}
	out, err := g.Run(ctx, "mount -t ext4 -o rw name:goos-bench /mnt/bench", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "ext4-mount", err)
	}
	if strings.Contains(out, "mount:") {
		return "", failf(ClassConfig, "ext4-mount", "the disk has no writable goos-bench partition:\n%s", out)
	}
	g.pass("ext4-mount")
	var measured []string
	for round := 1; round <= ext4BenchRounds; round++ {
		out, ending, err := runWorkloadStep(ctx, g, "/system/linux/fsbench /mnt/bench", ext4BenchTimeout)
		if err != nil {
			return "", err
		}
		if ending != "" || !hasLine(out, "bench ok") {
			return "", failf(ClassScenario, "fsbench", "round %d did not finish (%s):\n%s", round, ending, tailLines(out, 20))
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "bench ") && line != "bench ok" {
				measured = append(measured, fmt.Sprintf("ext4 round=%d %s", round, line))
				s.logf("round %d %s", round, line)
			}
		}
		g.pass(fmt.Sprintf("fsbench-%d", round))
	}
	// One round on RAMFS and one on the persistent FAT32 volume separate
	// what the VFS and the Linux ABI cost from what ext4 adds.
	for _, base := range []struct{ name, dir string }{{"ramfs", "/tmp"}, {"fat32", "/persist/home"}} {
		out, ending, err := runWorkloadStep(ctx, g, "/system/linux/fsbench "+base.dir, ext4BenchTimeout)
		if err != nil {
			return "", err
		}
		if ending != "" {
			return "", failf(ClassScenario, "fsbench-"+base.name, "%s did not finish (%s):\n%s", base.dir, ending, tailLines(out, 20))
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "bench ") && line != "bench ok" {
				measured = append(measured, fmt.Sprintf("%s %s", base.name, line))
				s.logf("%s %s", base.name, line)
			}
		}
	}
	out, err = g.Run(ctx, "umount /mnt/bench", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "ext4-umount", err)
	}
	if strings.Contains(out, "umount:") {
		return "", failf(ClassScenario, "ext4-umount", "%s", out)
	}
	g.pass("ext4-umount")
	if s.EvidenceDir != "" {
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "ext4-bench.txt"), []byte(strings.Join(measured, "\n")+"\n"), 0o600); err != nil {
			return "", fail(ClassInfra, "ext4-bench", err)
		}
	}
	return ResultPass, nil
}

// tailLines returns the last count lines of text.
func tailLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}
