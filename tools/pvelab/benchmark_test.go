package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestParseBenchLines(t *testing.T) {
	out := "netbench 8\nbench native-tcp-stream bytes=8388608 us=1000000 MiBps=8\nbench tcp-request-reply rounds=2000 us-per-round=815\n" +
		"remote-serve done 201\nbench ok\nbench native ok\n"
	lines := parseBenchLines(out)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	v, unit, ok := lines[0].value()
	if !ok || unit != "MiB/s" || v != 8 {
		t.Errorf("stream value = %v %q %v, want 8 MiB/s", v, unit, ok)
	}
	v, unit, ok = lines[1].value()
	if !ok || unit != "us" || v != 815 {
		t.Errorf("request-reply value = %v %q %v, want 815 us", v, unit, ok)
	}
}

func TestStreamValueKeepsFraction(t *testing.T) {
	line := parseBenchLines("bench tcp-stream bytes=33554432 us=4571428 MiBps=7")[0]
	if v, _, _ := line.value(); math.Abs(v-7) > 0.01 || v == 7 {
		t.Errorf("value = %v, want the unrounded rate near 7", v)
	}
}

func benchRuns(warmups, runs int, line func(i int) string) []BenchRun {
	var out []BenchRun
	for i := 0; i < warmups+runs; i++ {
		out = append(out, BenchRun{Index: i, Warmup: i < warmups, Lines: strings.Split(line(i), "\n")})
	}
	return out
}

func TestSummarizeUsesMedianOfMeasuredRuns(t *testing.T) {
	runs := benchRuns(2, 9, func(i int) string {
		// Warm-ups are slow; measured rounds take 100+i us.
		rr := 1000
		if i >= 2 {
			rr = 100 + (i-2)*10
		}
		return fmt.Sprintf("bench tcp-request-reply rounds=2000 us-per-round=%d\nbench native-v3-tcp-request-reply rounds=2000 us-per-round=%d\nbench native-v2-tcp-request-reply rounds=2000 us-per-round=900\n"+
			"bench tcp-stream bytes=1048576 us=1000 MiBps=1000\nbench native-v3-tcp-stream bytes=1048576 us=500 MiBps=2000\nbench native-v2-tcp-stream bytes=1048576 us=5000 MiBps=200", rr, rr/2)
	})
	r := summarize(BenchEnvironment{Variant: "hz100-opt2"}, runs)
	var rr *BenchMetric
	for i := range r.Metrics {
		if r.Metrics[i].Name == "tcp-request-reply" {
			rr = &r.Metrics[i]
		}
	}
	if rr == nil || len(rr.Samples) != 9 || len(rr.Warmup) != 2 {
		t.Fatalf("metric %+v, want 9 samples and 2 warm-ups", rr)
	}
	if rr.Median != 140 || rr.Min != 100 || rr.Max != 180 {
		t.Errorf("median/min/max = %v/%v/%v, want 140/100/180", rr.Median, rr.Min, rr.Max)
	}
	if len(r.Comparisons) != 2 {
		t.Fatalf("comparisons %+v, want 2", r.Comparisons)
	}
	for _, c := range r.Comparisons {
		switch c.Linux {
		case "tcp-request-reply":
			if c.Overhead != 70 {
				t.Errorf("request-reply overhead %v, want 70", c.Overhead)
			}
		case "tcp-stream":
			// 1000 MiB/s is 62.5 us per 64 KiB; 2000 MiB/s is 31.25.
			if math.Abs(c.Overhead-31.25) > 1e-9 {
				t.Errorf("stream overhead %v, want 31.25 us per 64 KiB", c.Overhead)
			}
		}
	}
	// Request-reply at 140 us meets both; stream at ~953 MiB/s meets both;
	// connect-close, UDP and poll-wake have no samples and miss.
	if r.HardMet || r.PreferredMet {
		t.Error("targets without samples must not count as met")
	}
	for _, target := range r.Targets {
		switch target.Metric {
		case "tcp-request-reply", "tcp-stream":
			if !target.PreferredMet || !target.HardMet {
				t.Errorf("%s: %+v, want met", target.Metric, target)
			}
		}
	}
	md := r.Markdown()
	for _, want := range []string{"# Linux network performance: hz100-opt2", "## Targets", "## Linux and native", "| linux | tcp-request-reply | us | 140 | 100 | 180 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back BenchReport
	if err := json.Unmarshal(data, &back); err != nil || len(back.Metrics) != len(r.Metrics) {
		t.Errorf("report does not survive JSON: %v", err)
	}
}

func TestTargetsEnvelope(t *testing.T) {
	lines := "bench tcp-stream bytes=1 us=1 MiBps=0\nbench tcp-request-reply rounds=1 us-per-round=175\nbench tcp-connect-close count=1 us-per-connection=240\n" +
		"bench udp-request-reply rounds=1 us-per-round=140\nbench poll-wake count=1 us-per-wake=240"
	runs := benchRuns(0, 1, func(int) string { return lines })
	// 1 byte in 1 us is about 0.95 MiB/s: the stream misses the envelope.
	r := summarize(BenchEnvironment{}, runs)
	if r.HardMet {
		t.Error("a slow stream must miss the hard envelope")
	}
	for _, target := range r.Targets {
		if target.Metric != "tcp-stream" && (!target.HardMet || target.PreferredMet) {
			t.Errorf("%s: %+v, want inside the envelope and short of the preferred target", target.Metric, target)
		}
	}
}

const diagSample = `DIAG begin tick=100 hz=100 requester=5
DIAG sched runnable=0x1 dead=0x0 slots=4 current=1 running=0x1 online=0xf switches=10 steals=2
DIAG lock level=ipc acquired=50 contended=1 spin=10 longest=5 stuck=0
DIAG lock level=sched acquired=70 contended=0 spin=0 longest=0 stuck=0
DIAG cpu index=0 ticks=100 idle=90 resched-ipis=3 shootdown-ipis=0
DIAG cpu index=1 ticks=100 idle=95 resched-ipis=4 shootdown-ipis=0
DIAG process pid=3 root=0x1000 free-handles=10
DIAG linux syscalls=1000 unsupported=0 last-unsupported=0 recent-unsupported= refused= bad-pointers=0
DIAG linux-net attaches=1 opens=4 bytes-sent=0 requests=20
DIAG event us=5 kind=wait pid=3 task=0x1 object=0x2 value=0x0
DIAG end
`

func TestDiagCountersAreGeneric(t *testing.T) {
	before := parseDiagCounters(diagSample)
	after := parseDiagCounters(strings.NewReplacer(
		"switches=10", "switches=74", "bytes-sent=0", "bytes-sent=655360 doorbells=7", "requests=20", "requests=180",
		"level=ipc acquired=50", "level=ipc acquired=90", "index=1 ticks=100", "index=1 ticks=164").Replace(diagSample))
	if _, ok := before["process"]; ok {
		t.Error("process lines list objects and must stay out")
	}
	if _, ok := before["linux"]["recent-unsupported"]; ok {
		t.Error("an empty list is not a counter")
	}
	if _, ok := before["sched"]["runnable"]; ok {
		t.Error("hexadecimal masks are not counters")
	}
	deltas := diffDiagCounters(before, after, 10)
	got := map[string]float64{}
	for _, d := range deltas {
		got[d.Line+" "+d.Field] = d.Normalised
	}
	want := map[string]float64{
		"sched switches": 6.4, "linux-net bytes-sent": 65536, "linux-net requests": 16,
		"lock level=ipc acquired": 4, "cpu index=1 ticks": 6.4, "cpu index=0 ticks": 0,
	}
	for key, value := range want {
		if v, ok := got[key]; !ok || math.Abs(v-value) > 1e-9 {
			t.Errorf("%s = %v (present %v), want %v", key, v, ok, value)
		}
	}
	// A counter new in the after dump has no before value and no delta.
	if _, ok := got["linux-net doorbells"]; ok {
		t.Error("a field missing before must not produce a delta")
	}
	// A later dump replaces an earlier one in the same output.
	if c := parseDiagCounters("DIAG begin tick=1\nDIAG sched switches=1\n" + diagSample); c["sched"]["switches"] != 10 {
		t.Errorf("switches = %d, want the last dump's 10", c["sched"]["switches"])
	}
}

func TestCompareBenchReportsFlagsSpread(t *testing.T) {
	make := func(hz int, rr, stream float64) *BenchReport {
		return &BenchReport{Environment: BenchEnvironment{HZ: hz, Variant: variantName(hz, 2, 0)}, Metrics: []BenchMetric{
			{Name: "tcp-request-reply", Group: "linux", Unit: "us", Median: rr},
			{Name: "tcp-stream", Group: "linux", Unit: "MiB/s", Median: stream},
		}}
	}
	md := compareBenchReports([]*BenchReport{make(100, 100, 50), make(250, 105, 51), make(1000, 120, 52)}, 0.10)
	if !strings.Contains(md, "| linux | tcp-request-reply | us | 100 | 105 | 120 | 20.0% | **differs** |") {
		t.Errorf("request-reply row missing or unflagged:\n%s", md)
	}
	if !strings.Contains(md, "| linux | tcp-stream | MiB/s | 50.0 | 51.0 | 52.0 | 4.0% |  |") {
		t.Errorf("stream row missing or flagged:\n%s", md)
	}
	if !strings.Contains(md, "1 metrics differ by more than 10% across HZ: tcp-request-reply.") {
		t.Errorf("summary missing:\n%s", md)
	}
}

func TestVariantName(t *testing.T) {
	if got := variantName(100, 2, 0); got != "hz100-opt2" {
		t.Errorf("got %q", got)
	}
	if got := variantName(1000, 2, 1); got != "hz1000-opt2-diag" {
		t.Errorf("got %q", got)
	}
}
