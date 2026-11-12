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

// ext4BenchScenario measures an ext4 volume mounted read-write (ADR 0226,
// ADR 0227) with fsbench, the static Linux program the Linux reference runs
// on the same volume layout. On ext4 it runs five full rounds and reads the
// system's settled page count after each, so a leak shows as growth from
// round to round; then 10000 names in one directory, then fills the volume
// and writes again after freeing it. One round on RAMFS, with a data file
// that fits /tmp, and one on the persistent FAT32 volume give comparisons.
// The disk carries a partition labelled goos-bench that
// tools/ext4fixture/make-bench-volume.sh made; make test-pve-ext4 builds it.
type ext4BenchScenario struct{}

// ext4BenchRounds is how many times the full fsbench runs on ext4; the
// report keeps every round so a reader can take the median.
const ext4BenchRounds = 5

// ext4BenchTimeout bounds one fsbench run.
const ext4BenchTimeout = 15 * time.Minute

// ext4PlateauPages is how far the settled page count after the last full
// round may rise above the highest count of the rounds before it: 1 MiB.
// Collected heaps settle a few hundred pages apart from run to run, up or
// down; a leak keeps rising. ext4KernelPages bounds the kernel's own
// growth over all rounds, which does not move with heaps.
const (
	ext4PlateauPages = 256
	ext4KernelPages  = 64
)

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
	record := func(label string, lines []string) {
		for _, line := range lines {
			measured = append(measured, label+" "+line)
			s.logf("%s %s", label, line)
		}
	}
	var pages, kernel []uint64
	for round := 1; round <= ext4BenchRounds; round++ {
		lines, err := runFsbench(ctx, g, "/mnt/bench", fmt.Sprintf("round %d", round))
		if err != nil {
			return "", err
		}
		record(fmt.Sprintf("ext4 round=%d", round), lines)
		used, inKernel, err := settledPages(ctx, g)
		if err != nil {
			return "", err
		}
		pages, kernel = append(pages, used), append(kernel, inKernel)
		record(fmt.Sprintf("ext4 round=%d", round), []string{fmt.Sprintf("mem pages=%d kernel=%d", used, inKernel)})
		g.pass(fmt.Sprintf("fsbench-%d", round))
	}
	highest := uint64(0)
	for _, used := range pages[:len(pages)-1] {
		highest = max(highest, used)
	}
	if last := pages[len(pages)-1]; last > highest+ext4PlateauPages {
		return "", failf(ClassScenario, "ext4-memory", "settled pages %v: the last round rose past the earlier ones", pages)
	}
	if kernel[len(kernel)-1] > kernel[0]+ext4KernelPages {
		return "", failf(ClassScenario, "ext4-memory", "kernel pages %v grew over the rounds", kernel)
	}
	g.pass("ext4-memory-plateau")

	// 10000 names in one directory and 8 MiB of data.
	lines, err := runFsbench(ctx, g, "/mnt/bench 5 8", "names")
	if err != nil {
		return "", err
	}
	record("ext4 names", lines)
	g.pass("fsbench-names")

	// Fill the volume, check the refusals, free it and write again.
	before, err := g.Run(ctx, "df", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "df", err)
	}
	lines, err = runFsbench(ctx, g, "/mnt/bench fill", "fill")
	if err != nil {
		return "", err
	}
	if !containsLine(lines, "bench refill ok=1") {
		return "", failf(ClassScenario, "fill", "no write after freeing the full volume:\n%s", strings.Join(lines, "\n"))
	}
	free, freeOK := dfFreeMiB(before, "/mnt/bench")
	filled, filledOK := fillMiB(lines)
	if !freeOK || !filledOK || filled*10 < free*9 {
		return "", failf(ClassScenario, "fill", "filled %d MiB of %d MiB free:\n%s", filled, free, strings.Join(lines, "\n"))
	}
	record("ext4 fill", lines)
	after, err := g.Run(ctx, "df", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "df", err)
	}
	record("ext4 df-before", dfLines(before, "/mnt/bench"))
	record("ext4 df-after", dfLines(after, "/mnt/bench"))
	g.pass("ext4-fill")
	used, inKernel, err := settledPages(ctx, g)
	if err != nil {
		return "", err
	}
	record("ext4 after-fill", []string{fmt.Sprintf("mem pages=%d kernel=%d", used, inKernel)})

	// One round on RAMFS and one on the persistent FAT32 volume separate
	// what the VFS and the Linux ABI cost from what ext4 adds. /tmp holds
	// 16 MiB, so its data file is 8 MiB.
	for _, base := range []struct{ name, args string }{{"ramfs", "/tmp 1 8"}, {"fat32", "/persist/home"}} {
		lines, err := runFsbench(ctx, g, base.args, base.name)
		if err != nil {
			return "", err
		}
		record(base.name, lines)
		g.pass("fsbench-" + base.name)
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

// runFsbench runs fsbench with args and returns its bench lines. A run that
// does not end with "bench ok" fails the scenario.
func runFsbench(ctx context.Context, g *Guest, args string, label string) ([]string, error) {
	out, ending, err := runWorkloadStep(ctx, g, "/system/linux/fsbench "+args, ext4BenchTimeout)
	if err != nil {
		return nil, err
	}
	if ending != "" || !hasLine(out, "bench ok") {
		return nil, failf(ClassScenario, "fsbench", "%s did not finish (%s):\n%s", label, ending, tailLines(out, 20))
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bench ") && line != "bench ok" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// settledPages reads the system's page count, and the kernel's part of it,
// once the collector settled, as the leak checks do; it retries an
// unsettled sample a few times.
func settledPages(ctx context.Context, g *Guest) (uint64, uint64, error) {
	for attempt := 0; attempt < 5; attempt++ {
		out, err := g.Run(ctx, "mem -p", commandTimeout)
		if err != nil {
			return 0, 0, fail(ClassScenario, "mem", err)
		}
		if !hasLine(out, "pages stable: yes") {
			time.Sleep(time.Second)
			continue
		}
		used, usedOK := memField(out, "pages used: ")
		inKernel, kernelOK := memField(out, "pages in the kernel: ")
		if !usedOK || !kernelOK {
			return 0, 0, failf(ClassScenario, "mem", "unexpected mem -p output:\n%s", out)
		}
		return used, inKernel, nil
	}
	return 0, 0, failf(ClassScenario, "mem", "the page count never settled")
}

// memField reads the number after prefix in mem -p output.
func memField(out string, prefix string) (uint64, bool) {
	for _, line := range strings.Split(out, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), prefix); found {
			number, err := strconv.ParseUint(value, 10, 64)
			return number, err == nil
		}
	}
	return 0, false
}

// dfLines returns the df lines that name mount.
func dfLines(out string, mount string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, mount) {
			lines = append(lines, "df "+strings.Join(strings.Fields(line), " "))
		}
	}
	return lines
}

// dfFreeMiB reads the free column of the df line for mount, given as
// 973.4M or 1.2G.
func dfFreeMiB(out string, mount string) (uint64, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[1] != mount {
			continue
		}
		value := fields[4]
		scale := 1.0
		switch {
		case strings.HasSuffix(value, "G"):
			scale = 1024
		case strings.HasSuffix(value, "M"):
		default:
			return 0, false
		}
		number, err := strconv.ParseFloat(value[:len(value)-1], 64)
		if err != nil {
			return 0, false
		}
		return uint64(number * scale), true
	}
	return 0, false
}

// fillMiB reads N from the "bench fill mib=N" line.
func fillMiB(lines []string) (uint64, bool) {
	for _, line := range lines {
		if rest, found := strings.CutPrefix(line, "bench fill mib="); found {
			number, err := strconv.ParseUint(strings.Fields(rest)[0], 10, 64)
			return number, err == nil
		}
	}
	return 0, false
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// tailLines returns the last count lines of text.
func tailLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}
