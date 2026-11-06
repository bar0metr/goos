package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The network benchmark (docs/development/proxmox-lab.md): in one VM it runs
// the Linux loopback bench (netprobe bench), the native GTCP and GUDP bench
// (netbench), and the host-guest bench (netprobe remote and remote-serve
// against benchpeer.py on the node), warm-up rounds first, then the measured
// rounds. It writes one JSON and one Markdown report per build variant.

//go:embed benchpeer.py
var benchPeerScript []byte

const (
	benchPeerPort  = 47200 // benchpeer.py TCP and UDP
	benchServePort = 47201 // netprobe remote-serve in the guest
	benchServeConn = 201   // the first connection stays out of the timing
	benchPeerLife  = 2 * 3600
	// benchSpacing separates the starts of two rounds. netd keeps a closed
	// connection's four-tuple for its 60 s TIME_WAIT and lets linuxsockd hold
	// about 320 of them; the 200 loopback connections of a round, repeated
	// sooner, would make socket() wait for one to expire.
	benchSpacing = 65 * time.Second
)

// BenchOptions are the settings make passes to the benchmark scenarios.
type BenchOptions struct {
	Variant    string
	HZ         int
	OPT        int
	DIAG       int
	Warmups    int
	Runs       int
	StreamMiB  int
	DiagRounds int
	Gate       bool
	ReportDir  string
	// Peer is the node's address on the lab bridge; empty means the SSH host.
	Peer   string
	Branch string
	// The stream stress: transfers in a row, each with its own watchdog,
	// then one long transfer (zero skips it).
	StressCount   int
	StressLongMiB int
	StressTimeout time.Duration
}

// variantName encodes the build settings a report depends on.
func variantName(hz, opt, diag int) string {
	name := fmt.Sprintf("hz%d-opt%d", hz, opt)
	if diag != 0 {
		name += "-diag"
	}
	return name
}

// NodeAccess runs commands on the Proxmox node and stages files in the
// run's staging directory, which cleanup removes with the run.
type NodeAccess interface {
	Run(ctx context.Context, argv ...string) ([]byte, error)
	Shell(ctx context.Context, script string) ([]byte, error)
	Stage(ctx context.Context, name string, data []byte) (string, error)
	Unstage(ctx context.Context) error
}

// benchLine is one "bench NAME key=value ..." line.
type benchLine struct {
	Name   string
	Fields map[string]string
}

// parseBenchLines reads the bench lines of a command's output. The closing
// "bench ok", "bench native ok" and "bench remote ok" lines carry no fields.
func parseBenchLines(text string) []benchLine {
	var out []benchLine
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		words := strings.Fields(raw)
		if len(words) < 3 || words[0] != "bench" || !strings.Contains(words[2], "=") {
			continue
		}
		line := benchLine{Name: words[1], Fields: map[string]string{}}
		for _, word := range words[2:] {
			if key, value, ok := strings.Cut(word, "="); ok {
				line.Fields[key] = value
			}
		}
		out = append(out, line)
	}
	return out
}

// value is a line's result: MiB/s from bytes and us when it has both, so a
// slow stream keeps its fraction, else its us-per-* field.
func (l benchLine) value() (float64, string, bool) {
	bytes, errB := strconv.ParseFloat(l.Fields["bytes"], 64)
	us, errU := strconv.ParseFloat(l.Fields["us"], 64)
	if errB == nil && errU == nil && us > 0 {
		return bytes / us * 1e6 / (1 << 20), "MiB/s", true
	}
	keys := make([]string, 0, len(l.Fields))
	for key := range l.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, "us-per-") {
			if v, err := strconv.ParseFloat(l.Fields[key], 64); err == nil {
				return v, "us", true
			}
		}
	}
	return 0, "", false
}

func benchGroup(name string) string {
	switch {
	case strings.HasPrefix(name, "native-v2-"):
		return "native GTCP 2"
	case strings.HasPrefix(name, "native-v4-"):
		return "native GTCP 4"
	case strings.HasPrefix(name, "native-u3-"):
		return "native GUDP 3"
	case strings.HasPrefix(name, "native-"):
		return "native"
	case strings.HasPrefix(name, "remote-"):
		return "host-guest"
	}
	return "linux"
}

// BenchMetric is every sample of one bench line across the runs.
type BenchMetric struct {
	Name    string    `json:"name"`
	Group   string    `json:"group"`
	Unit    string    `json:"unit"`
	Higher  bool      `json:"higher_is_better"`
	Warmup  []float64 `json:"warmup_samples"`
	Samples []float64 `json:"samples"`
	Median  float64   `json:"median"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
}

// BenchRun is the raw output of one round.
type BenchRun struct {
	Index  int      `json:"index"`
	Warmup bool     `json:"warmup"`
	Lines  []string `json:"lines"`
}

type BenchComparison struct {
	Linux        string  `json:"linux"`
	Native       string  `json:"native"`
	Unit         string  `json:"unit"`
	LinuxMedian  float64 `json:"linux_median"`
	NativeMedian float64 `json:"native_median"`
	// Overhead is Linux minus native in microseconds: per operation, or per
	// 64 KiB moved for a stream.
	Overhead     float64 `json:"overhead_us"`
	OverheadUnit string  `json:"overhead_unit"`
}

type BenchTarget struct {
	Metric       string  `json:"metric"`
	Unit         string  `json:"unit"`
	Higher       bool    `json:"higher_is_better"`
	Measured     bool    `json:"measured"`
	Median       float64 `json:"median"`
	Preferred    float64 `json:"preferred"`
	Hard         float64 `json:"hard"`
	PreferredMet bool    `json:"preferred_met"`
	HardMet      bool    `json:"hard_met"`
}

type BenchEnvironment struct {
	Branch         string    `json:"branch"`
	SHA            string    `json:"sha"`
	Dirty          bool      `json:"dirty"`
	Variant        string    `json:"variant"`
	HZ             int       `json:"hz"`
	OPT            int       `json:"opt"`
	DIAG           int       `json:"diag"`
	GuestCPU       string    `json:"guest_cpu"`
	Sockets        int       `json:"sockets"`
	Cores          int       `json:"cores"`
	VCPUs          int       `json:"vcpus"`
	MemoryMiB      int       `json:"memory_mib"`
	QEMU           string    `json:"qemu"`
	HostCPU        string    `json:"host_cpu"`
	Proxmox        string    `json:"proxmox"`
	NodeKernel     string    `json:"node_kernel"`
	RunID          string    `json:"run_id"`
	Started        time.Time `json:"started"`
	GuestAddress   string    `json:"guest_address,omitempty"`
	PeerAddress    string    `json:"peer_address,omitempty"`
	StreamMiB      int       `json:"stream_mib"`
	Warmups        int       `json:"warmups"`
	Runs           int       `json:"runs"`
	DiagRounds     int       `json:"diag_rounds,omitempty"`
	PeerNote       string    `json:"peer_note,omitempty"`
	MeasuredSample string    `json:"sample_rule"`
}

type BenchReport struct {
	Environment  BenchEnvironment  `json:"environment"`
	Metrics      []BenchMetric     `json:"metrics"`
	Comparisons  []BenchComparison `json:"native_vs_linux"`
	Targets      []BenchTarget     `json:"targets"`
	PreferredMet bool              `json:"preferred_met"`
	HardMet      bool              `json:"hard_met"`
	Runs         []BenchRun        `json:"runs"`
	Diag         *DiagPass         `json:"diag,omitempty"`
}

// benchTargets are the Linux loopback goals: preferred, then the hard
// envelope BENCH_GATE=1 enforces.
var benchTargets = []struct {
	metric          string
	higher          bool
	preferred, hard float64
}{
	{"tcp-stream", true, 272, 258},
	{"tcp-request-reply", false, 169, 178},
	{"tcp-connect-close", false, 233, 245},
	{"udp-request-reply", false, 137, 144},
	{"poll-wake", false, 236, 248},
}

// benchPairs set each Linux loopback metric beside a native one: GTCP 3
// for TCP, and for UDP GUDP 3, the protocol linuxsockd speaks. The GTCP 2 lines of netbench stay in the report on their own, as
// what an ordinary command's session costs.
var benchPairs = [][2]string{
	{"tcp-stream", "native-v3-tcp-stream"},
	{"tcp-request-reply", "native-v3-tcp-request-reply"},
	{"tcp-connect-close", "native-v3-tcp-connect-close"},
	{"tcp-socket", "native-v3-tcp-open"},
	{"tcp-connect", "native-v3-tcp-connect"},
	{"tcp-accept", "native-v3-tcp-accept"},
	{"tcp-close-client", "native-v3-tcp-close-client"},
	{"tcp-close-server", "native-v3-tcp-close-server"},
	{"udp-request-reply", "native-u3-udp-request-reply"},
	{"poll-wake", "native-v3-wait-wake"},
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// summarize builds the metrics, comparisons and target verdicts from the
// raw runs. Only measured runs count; warm-up samples are kept apart.
func summarize(env BenchEnvironment, runs []BenchRun) *BenchReport {
	r := &BenchReport{Environment: env, Runs: runs}
	index := map[string]int{}
	for _, run := range runs {
		for _, line := range parseBenchLines(strings.Join(run.Lines, "\n")) {
			v, unit, ok := line.value()
			if !ok {
				continue
			}
			i, seen := index[line.Name]
			if !seen {
				i = len(r.Metrics)
				index[line.Name] = i
				r.Metrics = append(r.Metrics, BenchMetric{Name: line.Name, Group: benchGroup(line.Name), Unit: unit, Higher: unit == "MiB/s"})
			}
			if run.Warmup {
				r.Metrics[i].Warmup = append(r.Metrics[i].Warmup, v)
			} else {
				r.Metrics[i].Samples = append(r.Metrics[i].Samples, v)
			}
		}
	}
	for i := range r.Metrics {
		m := &r.Metrics[i]
		if len(m.Samples) == 0 {
			continue
		}
		m.Median = median(m.Samples)
		m.Min, m.Max = m.Samples[0], m.Samples[0]
		for _, v := range m.Samples {
			m.Min, m.Max = math.Min(m.Min, v), math.Max(m.Max, v)
		}
	}
	find := func(name string) *BenchMetric {
		if i, ok := index[name]; ok && len(r.Metrics[i].Samples) > 0 {
			return &r.Metrics[i]
		}
		return nil
	}
	for _, pair := range benchPairs {
		linux, native := find(pair[0]), find(pair[1])
		if linux == nil || native == nil {
			continue
		}
		c := BenchComparison{Linux: linux.Name, Native: native.Name, Unit: linux.Unit, LinuxMedian: linux.Median, NativeMedian: native.Median}
		if linux.Unit == "MiB/s" {
			// Time per 64 KiB is 62500 us divided by the rate in MiB/s.
			c.Overhead, c.OverheadUnit = 62500/linux.Median-62500/native.Median, "us per 64 KiB"
		} else {
			c.Overhead, c.OverheadUnit = linux.Median-native.Median, "us per operation"
		}
		r.Comparisons = append(r.Comparisons, c)
	}
	r.PreferredMet, r.HardMet = true, true
	for _, target := range benchTargets {
		t := BenchTarget{Metric: target.metric, Higher: target.higher, Preferred: target.preferred, Hard: target.hard, Unit: "us"}
		if target.higher {
			t.Unit = "MiB/s"
		}
		if m := find(target.metric); m != nil {
			t.Measured, t.Median = true, m.Median
			if target.higher {
				t.PreferredMet, t.HardMet = m.Median >= target.preferred, m.Median >= target.hard
			} else {
				t.PreferredMet, t.HardMet = m.Median <= target.preferred, m.Median <= target.hard
			}
		}
		r.PreferredMet = r.PreferredMet && t.PreferredMet
		r.HardMet = r.HardMet && t.HardMet
		r.Targets = append(r.Targets, t)
	}
	return r
}

func benchNumber(v float64) string {
	switch {
	case math.IsNaN(v):
		return "n/a"
	case math.Abs(v) >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func verdict(met bool) string {
	if met {
		return "met"
	}
	return "**not met**"
}

func (r *BenchReport) Markdown() string {
	var b strings.Builder
	e := r.Environment
	fmt.Fprintf(&b, "# Linux network performance: %s\n\n", e.Variant)
	if e.DIAG != 0 {
		b.WriteString("A DIAG=1 build. Its timings carry the diagnostic counters' cost and are not primary results.\n\n")
	}
	b.WriteString("| | |\n|---|---|\n")
	dirty := ""
	if e.Dirty {
		dirty = " (dirty)"
	}
	rows := [][2]string{
		{"branch", fmt.Sprintf("%s at %s%s", e.Branch, e.SHA, dirty)},
		{"build", fmt.Sprintf("HZ=%d OPT=%d DIAG=%d VALIDATION=0", e.HZ, e.OPT, e.DIAG)},
		{"VM", fmt.Sprintf("cpu %s, %d socket x %d cores = %d vCPU, %d MiB", e.GuestCPU, e.Sockets, e.Cores, e.VCPUs, e.MemoryMiB)},
		{"QEMU", e.QEMU},
		{"host CPU", e.HostCPU},
		{"Proxmox", e.Proxmox + ", " + e.NodeKernel},
		{"run", fmt.Sprintf("%s, %s", e.RunID, e.Started.Format(time.RFC3339))},
		{"rounds", fmt.Sprintf("%d warm-up, %d measured; %s", e.Warmups, e.Runs, e.MeasuredSample)},
		{"stream size", fmt.Sprintf("%d MiB", e.StreamMiB)},
	}
	if e.PeerAddress != "" {
		rows = append(rows, [2]string{"host-guest", fmt.Sprintf("guest %s, peer %s; %s", e.GuestAddress, e.PeerAddress, e.PeerNote)})
	}
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s | %s |\n", row[0], row[1])
	}
	if len(r.Targets) > 0 && e.DIAG == 0 {
		b.WriteString("\n## Targets\n\nLinux loopback medians against the preferred targets and the hard envelope.\n\n")
		b.WriteString("| metric | median | preferred | | hard | |\n|---|---:|---:|---|---:|---|\n")
		for _, t := range r.Targets {
			op := "<="
			if t.Higher {
				op = ">="
			}
			median := "not measured"
			if t.Measured {
				median = benchNumber(t.Median) + " " + t.Unit
			}
			fmt.Fprintf(&b, "| %s | %s | %s %s | %s | %s %s | %s |\n", t.Metric, median, op, benchNumber(t.Preferred), verdict(t.PreferredMet), op, benchNumber(t.Hard), verdict(t.HardMet))
		}
		fmt.Fprintf(&b, "\nPreferred targets: %s. Hard envelope: %s.\n", verdict(r.PreferredMet), verdict(r.HardMet))
	}
	if len(r.Comparisons) > 0 {
		b.WriteString("\n## Linux and native\n\nThe adapter overhead is the Linux median minus the native median.\n\n")
		b.WriteString("| Linux | native | Linux median | native median | Linux minus native |\n|---|---|---:|---:|---:|\n")
		for _, c := range r.Comparisons {
			fmt.Fprintf(&b, "| %s | %s | %s %s | %s %s | %s %s |\n", c.Linux, c.Native, benchNumber(c.LinuxMedian), c.Unit, benchNumber(c.NativeMedian), c.Unit, benchNumber(c.Overhead), c.OverheadUnit)
		}
	}
	if len(r.Metrics) > 0 {
		b.WriteString("\n## Every metric\n\nMedian, minimum and maximum of the measured rounds; the samples in run order.\n\n")
		b.WriteString("| group | metric | unit | median | min | max | samples |\n|---|---|---|---:|---:|---:|---|\n")
		for _, m := range r.Metrics {
			samples := make([]string, len(m.Samples))
			for i, v := range m.Samples {
				samples[i] = benchNumber(v)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", m.Group, m.Name, m.Unit, benchNumber(m.Median), benchNumber(m.Min), benchNumber(m.Max), strings.Join(samples, ", "))
		}
	}
	if r.Diag != nil {
		b.WriteString(r.Diag.Markdown())
	}
	return b.String()
}

// write stores the report as <dir>/linux-network-perf-<variant>.json and .md.
func (r *BenchReport) write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, "linux-network-perf-"+r.Environment.Variant)
	if err := writeFileAtomic(base+".json", jsonIndent(r)); err != nil {
		return "", err
	}
	return base + ".md", writeFileAtomic(base+".md", []byte(r.Markdown()))
}

// benchEnvironment collects what makes a report comparable with another.
func benchEnvironment(ctx context.Context, s *Session) BenchEnvironment {
	o := s.Bench
	rep := s.Report
	e := BenchEnvironment{
		Branch: o.Branch, SHA: rep.Git.SHA, Dirty: rep.Git.Dirty, Variant: o.Variant, HZ: o.HZ, OPT: o.OPT, DIAG: o.DIAG,
		GuestCPU: rep.VM.CPU, Sockets: rep.VM.Sockets, Cores: rep.VM.Cores, VCPUs: rep.VM.Sockets * rep.VM.Cores, MemoryMiB: rep.VM.MemoryMiB,
		Proxmox: rep.Proxmox.Version, RunID: rep.RunID, Started: rep.StartedAt, StreamMiB: o.StreamMiB, Warmups: o.Warmups, Runs: o.Runs,
		MeasuredSample: "median of the measured rounds, never the best; rounds start at least 65 s apart so TIME_WAIT from one round has expired",
	}
	if s.Node == nil {
		return e
	}
	text := func(script string) string {
		out, err := s.Node.Shell(ctx, script)
		if err != nil {
			return "unknown"
		}
		return strings.TrimSpace(string(out))
	}
	e.QEMU = text("kvm --version 2>/dev/null | head -n 1 || qemu-system-x86_64 --version | head -n 1")
	e.HostCPU = text("lscpu | sed -n 's/^Model name: *//p' | head -n 1")
	e.NodeKernel = text("uname -sr")
	return e
}

// --- scenarios ------------------------------------------------------------

// linuxNetworkBenchScenario is make bench-pve-linux-network.
type linuxNetworkBenchScenario struct{}

func (linuxNetworkBenchScenario) Name() string { return "linux-network-bench" }

func (linuxNetworkBenchScenario) Run(ctx context.Context, s *Session) (Result, error) {
	o := s.Bench
	if o == nil || o.Runs < 1 {
		return "", failf(ClassConfig, "bench", "the benchmark needs --bench-runs and the other --bench flags make passes")
	}
	guest, err := benchPrepare(ctx, s)
	if err != nil {
		return "", err
	}
	env := benchEnvironment(ctx, s)
	env.GuestAddress, env.PeerAddress = guest, s.Bench.Peer
	env.PeerNote = "the guest is the client for streams and request-reply; benchpeer.py on the node is the client for accept-close and its Python time is part of each connection"
	if s.Node == nil {
		return "", failf(ClassConfig, "bench", "the host-guest bench needs SSH to the node")
	}
	peerDir, err := benchStartPeer(ctx, s)
	if err != nil {
		return "", err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.Node.Unstage(cctx)
	}()

	var runs []BenchRun
	var began time.Time
	reconnects := 0
	for i := 0; i < o.Warmups+o.Runs; i++ {
		if wait := time.Until(began.Add(benchSpacing)); i > 0 && wait > 0 {
			select {
			case <-ctx.Done():
				return "", fail(ClassInfra, "bench", ctx.Err())
			case <-time.After(wait):
			}
		}
		began = time.Now()
		run, err := benchRound(ctx, s, peerDir, guest, i)
		if err != nil && errors.Is(err, errConsoleClosed) && s.ReconnectSerial != nil && reconnects < 2 {
			// Someone opening the VM's serial terminal in the Proxmox UI
			// takes the console over. Attach again and repeat the round.
			reconnects++
			s.progress("round %d: %v; attaching the console again and repeating the round", i+1, err)
			if rerr := s.ReconnectSerial(ctx); rerr != nil {
				return "", fail(ClassInfra, "console", rerr)
			}
			i--
			continue
		}
		if err != nil {
			return "", err
		}
		label := "measured"
		if run.Warmup {
			label = "warm-up"
		}
		s.progress("round %d of %d (%s): %s", i+1, o.Warmups+o.Runs, label, strings.Join(run.Lines, "; "))
		runs = append(runs, run)
		// A later failure leaves the rounds so far in the run directory.
		if s.EvidenceDir != "" {
			writeRunFile(s.EvidenceDir, "linux-network-bench-runs.json", runs)
		}
	}
	report := summarize(env, runs)
	path, err := report.write(o.ReportDir)
	if err != nil {
		return "", fail(ClassInfra, "bench-report", err)
	}
	s.progress("report %s", path)
	if s.EvidenceDir != "" {
		writeRunFile(s.EvidenceDir, "linux-network-perf.json", report)
	}
	if o.Gate && !report.HardMet {
		return "", failf(ClassScenario, "bench-gate", "the Linux loopback medians miss the hard envelope; see %s", path)
	}
	return ResultPass, nil
}

// benchRound runs the three benchmarks and the accept-close exchange once.
func benchRound(ctx context.Context, s *Session, peerDir, guest string, index int) (BenchRun, error) {
	o, g := s.Bench, s.Guest
	size := strconv.Itoa(o.StreamMiB)
	run := BenchRun{Index: index, Warmup: index < o.Warmups}
	steps := []struct{ check, command, done string }{
		{"netprobe-bench", "/system/linux/netprobe bench " + size, "bench ok"},
		{"netbench", "netbench " + size, "bench native ok"},
		{"netbench3", "netbench3 " + size, "bench native ok"},
		{"netbench4", "netbench4 " + size, "bench native ok"},
		{"netprobe-remote", fmt.Sprintf("/system/linux/netprobe remote %s %d %s", o.Peer, benchPeerPort, size), "bench remote ok"},
	}
	for _, step := range steps {
		out, err := g.Run(ctx, step.command, netprobeTimeout)
		if err != nil {
			return run, fail(ClassScenario, step.check, err)
		}
		if err := g.Expect(step.check, out, step.done); err != nil {
			// netd's endpoints and sessions say what a stuck request waited
			// for; they reach the serial log through netd's log.
			for _, command := range []string{"net endpoints", "sleep 2"} {
				if _, dumpErr := g.Run(ctx, command, commandTimeout); dumpErr != nil {
					break
				}
			}
			return run, err
		}
		run.Lines = append(run.Lines, benchOutput(out)...)
	}
	// The node logs the sender's TCP_INFO once the guest closed the
	// stream; it may still be finishing.
	for attempt := 0; attempt < 20; attempt++ {
		out, err := s.Node.Shell(ctx, fmt.Sprintf("grep tcpinfo %s/peer.log | tail -n 1", shellQuote(peerDir)))
		if err == nil && len(out) != 0 {
			run.Lines = append(run.Lines, "host "+strings.TrimSpace(string(out)))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	served, err := benchAcceptClose(ctx, s, peerDir, guest)
	if err != nil {
		return run, err
	}
	run.Lines = append(run.Lines, served...)
	return run, nil
}

// benchOutput keeps the bench lines of a command's output.
func benchOutput(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bench ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// benchPrepare checks the image, waits for the lease, and returns the
// guest's address. It fills in the peer address from the SSH host and
// checks that the guest reaches it.
func benchPrepare(ctx context.Context, s *Session) (string, error) {
	g := s.Guest
	listing, err := g.Run(ctx, "ls /system/linux", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "bench", err)
	}
	if !strings.Contains(listing, "netprobe") {
		return "", failf(ClassConfig, "bench", "/system/linux/netprobe is missing")
	}
	if s.Report.VM.NetBridge == "" {
		return "", failf(ClassInfra, "bench", "PVE_NET_BRIDGE is empty, so the VM has no network function")
	}
	status, err := awaitLease(ctx, g)
	if err != nil {
		return "", err
	}
	fields := statusFields(status)
	guest, _, _ := strings.Cut(fields["address"], "/")
	s.logf("address %s, gateway %s", fields["address"], fields["gateway"])
	if s.Bench.DIAG != 0 {
		return guest, nil
	}
	if s.Bench.Peer == "" {
		return "", failf(ClassConfig, "bench", "no peer address; set --bench-peer or PVE_SSH_HOST to the node's bridge address")
	}
	out, err := g.Run(ctx, "ping -c 2 -W 1000 "+s.Bench.Peer, commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "ping peer", err)
	}
	if _, received, ok := pingSummary(out); !ok || received == 0 {
		return "", failf(ClassScenario, "ping peer", "the guest at %s does not reach the node at %s:\n%s", guest, s.Bench.Peer, out)
	}
	g.pass("bench-peer-reachable")
	return guest, nil
}

// benchStartPeer stages benchpeer.py and starts it on the node. It lives in
// the run's staging directory and ends with it, on SIGTERM, or after
// benchPeerLife seconds, whichever comes first.
func benchStartPeer(ctx context.Context, s *Session) (string, error) {
	if out, err := s.Node.Shell(ctx, "command -v python3"); err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return "", failf(ClassInfra, "bench-peer", "python3 is missing on the node: %v", err)
	}
	script, err := s.Node.Stage(ctx, "benchpeer.py", benchPeerScript)
	if err != nil {
		return "", fail(ClassInfra, "bench-peer", err)
	}
	dir := filepath.Dir(script)
	start := fmt.Sprintf("cd %s && (setsid nohup python3 benchpeer.py serve %d %s %d > peer.log 2>&1 < /dev/null &)",
		shellQuote(dir), benchPeerPort, shellQuote(dir+"/peer.pid"), benchPeerLife)
	if _, err := s.Node.Shell(ctx, start); err != nil {
		return "", fail(ClassInfra, "bench-peer", err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		out, _ := s.Node.Shell(ctx, "cat "+shellQuote(dir+"/peer.log"))
		if strings.Contains(string(out), "benchpeer listening") {
			s.logf("benchpeer on %s:%d", s.Bench.Peer, benchPeerPort)
			return dir, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := s.Node.Shell(ctx, "cat "+shellQuote(dir+"/peer.log"))
	return "", failf(ClassInfra, "bench-peer", "benchpeer.py did not start:\n%s", out)
}

// benchAcceptClose starts netprobe remote-serve in the guest's background
// and has benchpeer.py connect to it from the node.
func benchAcceptClose(ctx context.Context, s *Session, peerDir, guest string) ([]string, error) {
	g := s.Guest
	if _, err := g.Run(ctx, fmt.Sprintf("/system/linux/netprobe remote-serve %d %d &", benchServePort, benchServeConn), commandTimeout); err != nil {
		return nil, fail(ClassScenario, "remote-serve", err)
	}
	out, err := s.Node.Shell(ctx, fmt.Sprintf("cd %s && python3 benchpeer.py client %s %d %d", shellQuote(peerDir), shellQuote(guest), benchServePort, benchServeConn))
	if err != nil {
		return nil, fail(ClassScenario, "remote-accept-close", err)
	}
	// The background job prints its lines and the shell redraws its prompt
	// after the command above returned. Read through the job's end and
	// drop the rest, or the next command would take a redrawn prompt for
	// its own.
	jobCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	tail, err := g.con.ReadUntil(jobCtx, []byte("] done"))
	if err != nil {
		return nil, g.readFailure(ClassScenario, "remote-serve", "] done", err)
	}
	select {
	case <-time.After(settleTime):
	case <-ctx.Done():
	}
	tail = append(tail, g.con.Drain()...)
	if !strings.Contains(string(tail), fmt.Sprintf("remote-serve done %d", benchServeConn)) {
		return nil, failf(ClassScenario, "remote-serve", "the guest server did not answer every connection:\n%s", tail)
	}
	return benchOutput(string(out)), nil
}

// --- diagnostic pass -------------------------------------------------------

// linuxNetworkDiagScenario is the BENCH_DIAG=1 pass: on a DIAG=1 build it
// reads the guest's diag output around one stream of known size and one
// batch of request-reply rounds and reports the change of every counter.
type linuxNetworkDiagScenario struct{}

func (linuxNetworkDiagScenario) Name() string { return "linux-network-diag" }

// DiagWindow is the change of every DIAG counter across one workload.
type DiagWindow struct {
	Workload string      `json:"workload"`
	Command  string      `json:"command"`
	Per      string      `json:"per"`
	Units    float64     `json:"units"`
	Deltas   []DiagDelta `json:"deltas"`
	Output   []string    `json:"output"`
}

// DiagDelta is one field's change across a window. Baseline is the same
// field's change across the baseline window, which costs a launch and the
// dumps around it and does almost no work; Normalised is the change less
// the baseline, per unit.
type DiagDelta struct {
	Line       string  `json:"line"`
	Field      string  `json:"field"`
	Before     uint64  `json:"before"`
	After      uint64  `json:"after"`
	Delta      int64   `json:"delta"`
	Baseline   int64   `json:"baseline"`
	Normalised float64 `json:"normalised"`
}

type DiagPass struct {
	Baseline string       `json:"baseline"`
	Windows  []DiagWindow `json:"windows"`
}

// diagBaseline is the window whose deltas every other window subtracts:
// one UDP round, a launch with next to no socket work.
const diagBaseline = "/system/linux/netprobe bench-udp 1"

// lessBaseline subtracts the baseline window's deltas from deltas and
// normalises the rest per unit.
func lessBaseline(deltas, baseline []DiagDelta, units float64) {
	base := make(map[[2]string]int64, len(baseline))
	for _, d := range baseline {
		base[[2]string{d.Line, d.Field}] = d.Delta
	}
	for index := range deltas {
		d := &deltas[index]
		d.Baseline = base[[2]string{d.Line, d.Field}]
		if units > 0 {
			d.Normalised = float64(d.Delta-d.Baseline) / units
		}
	}
}

// diagDump takes netd's counters (net endpoints, which netd logs) and the
// kernel's dump. netd's records reach the console through the log, so a
// pause lets them land before the dump.
func (s *Session) diagDump(ctx context.Context) (diagCounters, error) {
	var netd string
	for _, command := range []string{"net status", "net endpoints", "sleep 1"} {
		out, err := s.Guest.Run(ctx, command, commandTimeout)
		if err != nil {
			return nil, fail(ClassScenario, "diag", err)
		}
		netd += out
	}
	out, err := s.Guest.Run(ctx, "diag", commandTimeout)
	if err != nil {
		return nil, fail(ClassScenario, "diag", err)
	}
	if !strings.Contains(out, "DIAG ") {
		return nil, failf(ClassConfig, "diag", "no DIAG lines; the diagnostic pass needs a DIAG=1 build:\n%s", out)
	}
	return parseDiagCounters(out + "\n" + netdDiagLines(netd)), nil
}

// netdDiagLines turns net status's frame counts and netd's counter records
// into DIAG lines: "netd
// loop loops=..." becomes "DIAG netd-loop loops=...". The endpoint and
// session records describe objects, not work, and stay out.
func netdDiagLines(out string) string {
	var b strings.Builder
	for _, raw := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "frames: "); ok {
			// net status: "frames: rx 2 tx 7 drop 0 malformed 0".
			words := strings.Fields(rest)
			b.WriteString("DIAG frames")
			for i := 0; i+1 < len(words); i += 2 {
				b.WriteString(" " + words[i] + "=" + words[i+1])
			}
			b.WriteString("\n")
			continue
		}
		_, record, found := strings.Cut(raw, "]: ")
		if !found || !strings.Contains(raw, "netd[") {
			continue
		}
		for _, name := range []string{"netd loop", "netd wait", "tcp counters", "tcp lookups"} {
			if rest, ok := strings.CutPrefix(record, name+" "); ok {
				b.WriteString("DIAG " + strings.ReplaceAll(name, " ", "-") + " " + rest + "\n")
			}
		}
	}
	return b.String()
}

func (linuxNetworkDiagScenario) Run(ctx context.Context, s *Session) (Result, error) {
	o := s.Bench
	if o == nil || o.DiagRounds < 1 || o.StreamMiB < 1 {
		return "", failf(ClassConfig, "bench", "the diagnostic pass needs --bench-stream-mib and --bench-diag-rounds")
	}
	guest, err := benchPrepare(ctx, s)
	if err != nil {
		return "", err
	}
	env := benchEnvironment(ctx, s)
	env.GuestAddress, env.Runs, env.Warmups, env.DiagRounds = guest, 1, 0, o.DiagRounds
	env.MeasuredSample = "one run of each workload, for counters only"
	windows := []struct {
		workload, command, per string
		units                  float64
	}{
		{"stream", fmt.Sprintf("/system/linux/netprobe bench-stream %d", o.StreamMiB), "64 KiB written", float64(o.StreamMiB) * 16},
		{"request-reply", fmt.Sprintf("/system/linux/netprobe bench-rr %d", o.DiagRounds), "request-reply round", float64(o.DiagRounds)},
		{"connect-close", "/system/linux/netprobe bench-connect 200", "connection", 200},
		{"udp-request-reply", fmt.Sprintf("/system/linux/netprobe bench-udp %d", o.DiagRounds), "UDP request-reply round", float64(o.DiagRounds)},
	}
	pass := &DiagPass{Baseline: diagBaseline}
	var lines []string
	var baseline []DiagDelta
	windows = append([]struct {
		workload, command, per string
		units                  float64
	}{{"baseline", diagBaseline, "run", 1}}, windows...)
	for _, w := range windows {
		before, err := s.diagDump(ctx)
		if err != nil {
			return "", err
		}
		out, err := s.Guest.Run(ctx, w.command, netprobeTimeout)
		if err != nil {
			return "", fail(ClassScenario, w.workload, err)
		}
		if err := s.Guest.Expect(w.workload, out, "bench ok"); err != nil {
			return "", err
		}
		after, err := s.diagDump(ctx)
		if err != nil {
			return "", err
		}
		deltas := diffDiagCounters(before, after, w.units)
		if w.workload == "baseline" {
			baseline = deltas
			continue
		}
		lessBaseline(deltas, baseline, w.units)
		measured := benchOutput(out)
		lines = append(lines, measured...)
		pass.Windows = append(pass.Windows, DiagWindow{Workload: w.workload, Command: w.command, Per: w.per, Units: w.units,
			Deltas: deltas, Output: measured})
	}
	report := summarize(env, []BenchRun{{Index: 0, Lines: lines}})
	report.Targets, report.Comparisons = nil, nil
	report.Diag = pass
	path, err := report.write(o.ReportDir)
	if err != nil {
		return "", fail(ClassInfra, "bench-report", err)
	}
	s.progress("report %s", path)
	return ResultPass, nil
}

func (p *DiagPass) Markdown() string {
	var b strings.Builder
	b.WriteString("\n## Counter deltas\n\nThe change of every decimal field on the DIAG lines across one workload, normalised per unit of work. Lines that repeat within a dump are told apart by their first field. Zero deltas are left out here and kept in the JSON.\n")
	if p.Baseline != "" {
		fmt.Fprintf(&b, "\nThe baseline column is the change across `%s`, a launch and the dumps around it with almost no socket work. The per-unit column is the delta less the baseline.\n", p.Baseline)
	}
	for _, w := range p.Windows {
		fmt.Fprintf(&b, "\n### %s\n\n`%s`: %s units of one %s. %s\n\n", w.Workload, w.Command, benchNumber(w.Units), w.Per, strings.Join(w.Output, "; "))
		fmt.Fprintf(&b, "| line | field | delta | baseline | per %s |\n|---|---|---:|---:|---:|\n", w.Per)
		for _, d := range w.Deltas {
			if d.Delta == 0 {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", d.Line, d.Field, d.Delta, d.Baseline, benchNumber(d.Normalised))
		}
	}
	return b.String()
}

// diagCounters maps a DIAG line's identity to its decimal fields.
type diagCounters map[string]map[string]uint64

// diagSkipped are DIAG lines that list objects rather than count work.
var diagSkipped = map[string]bool{"event": true, "events": true, "waitany": true, "interest": true, "handle": true, "channel": true, "end": true}

// addProcessTime adds the run-us fields of one DIAG process line to its
// process name's total.
func addProcessTime(total map[string]uint64, words []string) {
	name := ""
	var us uint64
	for _, w := range words {
		k, v, _ := strings.Cut(w, "=")
		switch k {
		case "name":
			name = v
		case "run-us":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				us += n
			}
		}
	}
	if name != "" {
		total[name] += us
	}
}

// parseDiagCounters reads every "DIAG name key=value ..." line of the last
// dump in text. It knows no field names: a counter the kernel adds later
// shows up without a change here. Hexadecimal values are masks and IDs, and
// lists are not counters, so both stay out.
func parseDiagCounters(text string) diagCounters {
	text = strings.ReplaceAll(text, "\r", "")
	if i := strings.LastIndex(text, "DIAG begin"); i >= 0 {
		text = text[i:]
	}
	type parsed struct {
		name   string
		fields [][2]string
	}
	var lines []parsed
	count := map[string]int{}
	processTime := map[string]uint64{}
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(raw, "DIAG ")
		if !ok {
			continue
		}
		words := strings.Fields(rest)
		if len(words) == 0 {
			continue
		}
		p := parsed{}
		if !strings.Contains(words[0], "=") {
			p.name, words = words[0], words[1:]
		}
		if diagSkipped[p.name] {
			continue
		}
		if p.name == "process" {
			// A process line names its tasks' CPU time; the time of every
			// process of one name adds up, so a workload shows where its
			// time went.
			addProcessTime(processTime, words)
			continue
		}
		for _, w := range words {
			if k, v, ok := strings.Cut(w, "="); ok && k != "" {
				p.fields = append(p.fields, [2]string{k, v})
			}
		}
		count[p.name]++
		lines = append(lines, p)
	}
	out := diagCounters{}
	for _, p := range lines {
		identity := p.name
		if count[p.name] > 1 && len(p.fields) > 0 {
			identity += " " + p.fields[0][0] + "=" + p.fields[0][1]
		}
		values := out[identity]
		if values == nil {
			values = map[string]uint64{}
			out[identity] = values
		}
		for _, f := range p.fields {
			if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				values[f[0]] = n
			}
		}
	}
	for name, us := range processTime {
		out["process name="+name] = map[string]uint64{"run-us": us}
	}
	return out
}

// diffDiagCounters subtracts before from after for every field both hold.
func diffDiagCounters(before, after diagCounters, units float64) []DiagDelta {
	var out []DiagDelta
	for identity, fields := range after {
		old, ok := before[identity]
		if !ok {
			continue
		}
		for key, value := range fields {
			was, ok := old[key]
			if !ok {
				continue
			}
			d := DiagDelta{Line: identity, Field: key, Before: was, After: value, Delta: int64(value - was)}
			if units > 0 {
				d.Normalised = float64(d.Delta) / units
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// --- HZ comparison ---------------------------------------------------------

// compareBenchReports sets the medians of reports side by side and flags a
// metric whose medians differ by more than limit (0.10 for 10%) between the
// lowest and the highest.
func compareBenchReports(reports []*BenchReport, limit float64) string {
	var b strings.Builder
	b.WriteString("# Linux network performance across HZ\n\n")
	b.WriteString("Medians of each report. A metric is flagged when its highest median exceeds its lowest by more than ")
	fmt.Fprintf(&b, "%.0f%%.\n\n", limit*100)
	b.WriteString("| | ")
	for _, r := range reports {
		fmt.Fprintf(&b, "%s | ", r.Environment.Variant)
	}
	b.WriteString("\n|---|")
	for range reports {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, row := range [][2]string{{"branch", "sha"}, {"vCPU", "memory"}} {
		fmt.Fprintf(&b, "| %s | ", row[0])
		for _, r := range reports {
			e := r.Environment
			if row[0] == "branch" {
				fmt.Fprintf(&b, "%s %.12s | ", e.Branch, e.SHA)
			} else {
				fmt.Fprintf(&b, "%d vCPU, %d MiB | ", e.VCPUs, e.MemoryMiB)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\n| group | metric | unit | ")
	for _, r := range reports {
		fmt.Fprintf(&b, "HZ=%d | ", r.Environment.HZ)
	}
	b.WriteString("spread | |\n|---|---|---|")
	for range reports {
		b.WriteString("---:|")
	}
	b.WriteString("---:|---|\n")
	var flagged []string
	if len(reports) == 0 {
		return b.String()
	}
	for _, m := range reports[0].Metrics {
		medians := make([]float64, 0, len(reports))
		for _, r := range reports {
			v := math.NaN()
			for _, other := range r.Metrics {
				if other.Name == m.Name {
					v = other.Median
				}
			}
			medians = append(medians, v)
		}
		low, high := math.Inf(1), math.Inf(-1)
		for _, v := range medians {
			if !math.IsNaN(v) {
				low, high = math.Min(low, v), math.Max(high, v)
			}
		}
		spread := math.NaN()
		if low > 0 && !math.IsInf(high, -1) {
			spread = (high - low) / low
		}
		mark := ""
		if spread > limit {
			mark = "**differs**"
			flagged = append(flagged, m.Name)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | ", m.Group, m.Name, m.Unit)
		for _, v := range medians {
			fmt.Fprintf(&b, "%s | ", benchNumber(v))
		}
		spreadText := "n/a"
		if !math.IsNaN(spread) {
			spreadText = fmt.Sprintf("%.1f%%", spread*100)
		}
		fmt.Fprintf(&b, "%s | %s |\n", spreadText, mark)
	}
	if len(flagged) == 0 {
		fmt.Fprintf(&b, "\nNo metric differs by more than %.0f%% across HZ.\n", limit*100)
	} else {
		fmt.Fprintf(&b, "\n%d metrics differ by more than %.0f%% across HZ: %s.\n", len(flagged), limit*100, strings.Join(flagged, ", "))
	}
	return b.String()
}

func (s *Session) progress(format string, args ...any) {
	if s.infof != nil {
		s.infof(format, args...)
		return
	}
	s.logf(format, args...)
}
