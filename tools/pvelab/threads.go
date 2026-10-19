package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// threadsScenario checks native threads under KVM (ADR 0203) and times the
// allocator with the heap lock alone and with thread-owned slabs. It needs a
// validation image, which carries threadprobe. The timings go to
// thread-allocator.md and thread-bench.txt in the run's evidence directory.
type threadsScenario struct{}

func (threadsScenario) Name() string { return "threads" }

// threadChecks run first; each must print its marker.
var threadChecks = []struct{ name, command, want string }{
	{"threads-smoke", "threadprobe smoke 7", "THREADPROBE_SMOKE_OK"},
	{"threads-lifecycle", "threadprobe lifecycle", "THREADPROBE_LIFECYCLE_OK"},
	{"threads-gc", "threadprobe gc 7 40", "THREADPROBE_GC_OK"},
	{"threads-roots", "threadprobe roots", "THREADPROBE_ROOTS_OK"},
	{"threads-storm", "threadprobe storm 7 20", "THREADPROBE_STORM_OK"},
}

const (
	threadBenchRuns = 5
	threadBenchOps  = 200000
)

var threadBenchCounts = []int{1, 2, 4, 8}

func (threadsScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	for _, check := range threadChecks {
		out, err := g.Run(ctx, check.command, commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, check.name, err)
		}
		if err := g.Expect(check.name, out, check.want); err != nil {
			return "", err
		}
		if err := g.Clean(check.name, out); err != nil {
			return "", err
		}
		g.pass(check.name)
	}
	var raw strings.Builder
	samples := map[benchKey][]benchSample{}
	for _, threads := range threadBenchCounts {
		for _, mode := range []string{"lock", "cache"} {
			for run := 0; run < threadBenchRuns; run++ {
				command := fmt.Sprintf("threadprobe bench %d %d %s", threads, threadBenchOps, mode)
				out, err := g.Run(ctx, command, 5*time.Minute)
				if err != nil {
					return "", fail(ClassScenario, "threads-bench", err)
				}
				if err := g.Expect("threads-bench", out, "THREADPROBE_BENCH_OK"); err != nil {
					return "", err
				}
				sample, ok := parseBench(out)
				if !ok {
					return "", failf(ClassScenario, "threads-bench", "no timing in:\n%s", out)
				}
				samples[benchKey{threads, mode}] = append(samples[benchKey{threads, mode}], sample)
				for _, line := range strings.Split(out, "\n") {
					if strings.HasPrefix(line, "THREADPROBE_BENCH ") || strings.HasPrefix(line, "ALLOC_PROFILE threadprobe threads ") {
						raw.WriteString(strings.TrimSpace(line) + "\n")
					}
				}
			}
		}
	}
	g.pass("threads-bench")
	cpus := s.Report.VM.Sockets * s.Report.VM.Cores
	if s.EvidenceDir != "" {
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "thread-bench.txt"), []byte(raw.String()), 0o600); err != nil {
			return "", failf(ClassInfra, "threads-bench", "writing evidence: %v", err)
		}
		table := benchTable(samples, cpus)
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "thread-allocator.md"), []byte(table), 0o600); err != nil {
			return "", failf(ClassInfra, "threads-bench", "writing evidence: %v", err)
		}
		s.logf("%s", table)
	}
	return ResultPass, nil
}

type benchKey struct {
	threads int
	mode    string
}

type benchSample struct {
	ns, collections, contended, refills, pauseMax uint64
}

// parseBench reads one bench run's timing line and profile.
func parseBench(out string) (benchSample, bool) {
	fields := map[string]uint64{}
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		bench := strings.HasPrefix(line, "THREADPROBE_BENCH ")
		if !bench && !strings.HasPrefix(line, "ALLOC_PROFILE threadprobe threads ") {
			continue
		}
		found = found || bench
		for _, word := range strings.Fields(line) {
			if key, value, ok := strings.Cut(word, "="); ok {
				if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
					fields[key] = parsed
				}
			}
		}
	}
	return benchSample{fields["ns"], fields["collections"], fields["heap_contended"], fields["refills"], fields["pause_max"]}, found && fields["ns"] != 0
}

// benchTable renders the median run of each thread count and mode.
func benchTable(samples map[benchKey][]benchSample, cpus int) string {
	median := func(list []benchSample) benchSample {
		sorted := append([]benchSample(nil), list...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].ns < sorted[b].ns })
		return sorted[len(sorted)/2]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Thread allocator benchmark\n\n%d processors under KVM, %d allocations per thread, median of %d runs.\n\n", cpus, threadBenchOps, threadBenchRuns)
	b.WriteString("| Threads | Heap lock ms | Owned slabs ms | Heap lock Mallocs/s | Owned slabs Mallocs/s | Speedup | Collections | Contended (lock) | Refills (slabs) | Longest pause us (slabs) |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, threads := range threadBenchCounts {
		lockRuns, cacheRuns := samples[benchKey{threads, "lock"}], samples[benchKey{threads, "cache"}]
		if len(lockRuns) == 0 || len(cacheRuns) == 0 {
			continue
		}
		lock, cache := median(lockRuns), median(cacheRuns)
		total := float64(threads * threadBenchOps)
		fmt.Fprintf(&b, "| %d | %.1f | %.1f | %.2f | %.2f | %.2fx | %d | %d | %d | %.0f |\n", threads,
			float64(lock.ns)/1e6, float64(cache.ns)/1e6, total/float64(lock.ns)*1e3, total/float64(cache.ns)*1e3,
			float64(lock.ns)/float64(cache.ns), cache.collections, lock.contended, cache.refills, float64(cache.pauseMax)/1e3)
	}
	return b.String()
}
