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

// threadsScenario checks native threads under KVM (ADR 0203), runs the
// allocator workloads of ADR 0205, and times the allocator with the heap
// lock alone and with thread caches. It needs a validation image, which
// carries threadprobe. The timings go to thread-allocator.md and
// thread-bench.txt, and the workloads' output to thread-workloads.txt, in
// the run's evidence directory.
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

// threadWorkloads are the allocator workloads: remote frees, a mostly live
// heap, bursts with idle waits, and allocation near the page quota. Their
// output goes to the evidence directory.
var threadWorkloads = []struct{ name, command, want string }{
	{"threads-remote-one", "threadprobe remote 2 200000 one", "THREADPROBE_REMOTE_OK"},
	{"threads-remote-two", "threadprobe remote 8 200000 two", "THREADPROBE_REMOTE_OK"},
	// Eight threads keeping 1024 or 2048 objects of the bench mix hold
	// about 2 or 4 MiB: a quarter or half of what they allocate between
	// collections at the 8 MiB floor. 8192 objects, about 15 MiB, outgrow
	// the floor, so the goal is twice the live heap.
	{"threads-live-25", "threadprobe live 8 200000 1024", "THREADPROBE_LIVE_OK"},
	{"threads-live-50", "threadprobe live 8 200000 2048", "THREADPROBE_LIVE_OK"},
	{"threads-live-heap", "threadprobe live 8 200000 8192", "THREADPROBE_LIVE_OK"},
	{"threads-idle", "threadprobe idle 8 20", "THREADPROBE_IDLE_OK"},
	{"threads-quota", "threadprobe quota 8 3", "THREADPROBE_QUOTA_OK"},
}

const (
	threadBenchRuns = 5
	threadBenchOps  = 200000
)

var threadBenchCounts = []int{1, 2, 4, 8}

// threadBenchModes are the heap lock alone, thread caches, thread caches
// with 256 free pages retained across collections, and thread caches on
// sizes of 512 to 2048 bytes only.
var threadBenchModes = []string{"lock", "cache", "retain", "big"}

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
	var workloads strings.Builder
	for _, check := range threadWorkloads {
		out, err := g.Run(ctx, check.command, 10*time.Minute)
		if err != nil {
			return "", fail(ClassScenario, check.name, err)
		}
		if err := g.Expect(check.name, out, check.want); err != nil {
			return "", err
		}
		fmt.Fprintf(&workloads, "$ %s\n%s\n", check.command, strings.TrimSpace(out))
		g.pass(check.name)
	}
	var raw strings.Builder
	samples := map[benchKey][]benchSample{}
	for _, threads := range threadBenchCounts {
		for _, mode := range threadBenchModes {
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
					if strings.HasPrefix(line, "THREADPROBE_BENCH ") || strings.HasPrefix(line, "ALLOC_PROFILE threadprobe ") {
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
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "thread-workloads.txt"), []byte(workloads.String()), 0o600); err != nil {
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
	ns, collections, contended, refills, pauseMax, pauseTotal uint64
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
	return benchSample{fields["ns"], fields["collections"], fields["heap_contended"], fields["refills"], fields["pause_max"], fields["pause_total"]},
		found && fields["ns"] != 0
}

// benchTable renders the median run of each mode and thread count: wall
// time, throughput, scaling against one thread of the same mode, speedup
// against the heap lock alone at the same count, and the shared-heap and
// collection work behind them.
func benchTable(samples map[benchKey][]benchSample, cpus int) string {
	median := func(list []benchSample) benchSample {
		sorted := append([]benchSample(nil), list...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].ns < sorted[b].ns })
		return sorted[len(sorted)/2]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Thread allocator benchmark\n\n%d processors under KVM, %d allocations per thread, median of %d runs.\n\n", cpus, threadBenchOps, threadBenchRuns)
	b.WriteString("| Mode | Threads | ms | Mallocs/s | ns/alloc | Scaling | vs lock | Collections | Contended | Refills/kalloc | Pause share | Longest pause us |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, mode := range threadBenchModes {
		var single float64
		for _, threads := range threadBenchCounts {
			runs := samples[benchKey{threads, mode}]
			if len(runs) == 0 {
				continue
			}
			m := median(runs)
			total := float64(threads * threadBenchOps)
			rate := total / float64(m.ns) * 1e9
			if threads == 1 {
				single = rate
			}
			scaling, speedup := "-", "-"
			if single != 0 {
				scaling = fmt.Sprintf("%.0f%%", rate/(float64(threads)*single)*100)
			}
			if lock := samples[benchKey{threads, "lock"}]; len(lock) != 0 {
				speedup = fmt.Sprintf("%.2fx", float64(median(lock).ns)/float64(m.ns))
			}
			fmt.Fprintf(&b, "| %s | %d | %.1f | %.0f | %.0f | %s | %s | %d | %d | %.2f | %.1f%% | %.0f |\n", mode, threads,
				float64(m.ns)/1e6, rate, float64(m.ns)/total, scaling, speedup, m.collections, m.contended,
				float64(m.refills)*1000/total, float64(m.pauseTotal)/float64(m.ns)*100, float64(m.pauseMax)/1e3)
		}
	}
	return b.String()
}
