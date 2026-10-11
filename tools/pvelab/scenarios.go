package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Serial markers. Scenarios use these names, never the literals.
const (
	shellPrompt  = "goos> "
	modeNormal   = "[MODE] normal"
	modeRecovery = "[MODE] recovery"
	kernelPanic  = "PANIC:"
	userFault    = "user fault:"
)

const (
	promptTimeout  = 60 * time.Second
	commandTimeout = 60 * time.Second
	settleTime     = time.Second
)

// Guest drives the GoOS shell over the serial console.
type Guest struct {
	con    Console
	checks []string
	logf   func(string, ...any)
}

func tailText(c Console) string {
	return strings.ReplaceAll(string(c.Tail(1500)), "\r", "")
}

func (g *Guest) pass(check string) {
	g.checks = append(g.checks, check)
	g.logf("check %s ok", check)
}

// readFailure turns a failed console read into a classified error.
func (g *Guest) readFailure(class Class, stage, waitingFor string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return failf(class, stage, "timeout waiting for %q; serial ends with:\n%s", waitingFor, tailText(g.con))
	}
	if errors.Is(err, context.Canceled) {
		return failf(ClassInfra, stage, "interrupted")
	}
	return failf(ClassInfra, stage, "serial console: %v", err)
}

// WaitBoot waits for the normal-mode marker and then the shell prompt.
// Recovery mode and a kernel panic are boot failures.
func (g *Guest) WaitBoot(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	which, _, err := g.con.ReadUntilAny(ctx, []byte(modeNormal), []byte(modeRecovery), []byte(kernelPanic))
	if err != nil {
		return g.readFailure(ClassBoot, "boot", modeNormal, err)
	}
	switch which {
	case 1:
		line, _ := g.con.ReadUntil(ctx, []byte("\n"))
		return failf(ClassBoot, "boot", "the system started in recovery mode%s", strings.TrimRight(string(line), "\r\n"))
	case 2:
		return failf(ClassBoot, "boot", "kernel panic:\n%s", tailText(g.con))
	}
	g.pass("boot-mode-normal")
	promptCtx, cancelPrompt := context.WithTimeout(ctx, promptTimeout)
	defer cancelPrompt()
	which, _, err = g.con.ReadUntilAny(promptCtx, []byte(shellPrompt), []byte(kernelPanic))
	if err != nil {
		return g.readFailure(ClassBoot, "shell", shellPrompt, err)
	}
	if which == 1 {
		return failf(ClassBoot, "shell", "kernel panic:\n%s", tailText(g.con))
	}
	// Late service lines may follow the prompt; let them land before the
	// first command.
	select {
	case <-time.After(settleTime):
	case <-ctx.Done():
	}
	if late := g.con.Drain(); strings.Contains(string(late), kernelPanic) {
		return failf(ClassBoot, "shell", "kernel panic:\n%s", tailText(g.con))
	}
	g.pass("shell-prompt")
	return nil
}

// Run sends one command line and returns its output without the echoed
// command and the next prompt.
func (g *Guest) Run(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if err := g.con.WriteLine(command); err != nil {
		return "", failf(ClassInfra, "console", "writing %q: %v", command, err)
	}
	return g.awaitPrompt(ctx, command, timeout)
}

func (g *Guest) awaitPrompt(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	which, out, err := g.con.ReadUntilAny(ctx, []byte("\n"+shellPrompt), []byte(kernelPanic))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return cleanOutput(command, string(out)), &promptTimeoutError{command: command, tail: tailText(g.con)}
		}
		return "", g.readFailure(ClassScenario, "command", shellPrompt, err)
	}
	if which == 1 {
		return "", failf(ClassBoot, "command", "kernel panic during %q:\n%s", command, tailText(g.con))
	}
	return cleanOutput(command, string(out)), nil
}

type promptTimeoutError struct {
	command string
	tail    string
}

func (e *promptTimeoutError) Error() string {
	return fmt.Sprintf("no prompt after %q; serial ends with:\n%s", e.command, e.tail)
}

// cleanOutput drops carriage returns, the echoed command, and the prompt.
func cleanOutput(command, raw string) string {
	text := strings.ReplaceAll(raw, "\r", "")
	text = strings.TrimSuffix(text, "\n"+shellPrompt)
	if first, rest, ok := strings.Cut(text, "\n"); ok && strings.TrimSpace(first) == strings.TrimSpace(command) {
		text = rest
	} else if !ok && strings.TrimSpace(text) == strings.TrimSpace(command) {
		text = ""
	}
	return text
}

// Expect requires a line of out to contain want.
func (g *Guest) Expect(check, out, want string) error {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			g.pass(check)
			return nil
		}
	}
	return failf(ClassScenario, check, "no line contains %q; output:\n%s", want, out)
}

// Clean refuses output that shows the command ended badly.
func (g *Guest) Clean(check, out string) error {
	for _, bad := range []string{"exit status:", "killed", "not found", "permission denied", userFault} {
		if strings.Contains(out, bad) {
			return failf(ClassScenario, check, "output contains %q:\n%s", bad, out)
		}
	}
	return nil
}

// Session is what a scenario works with.
type Session struct {
	Guest       *Guest
	Report      *Report
	RequirePass bool
	// KeepRunning leaves the VM up for a kept run; smoke then skips poweroff.
	KeepRunning     bool
	WorkloadTimeout time.Duration
	// PowerState reports the VM's state as Proxmox sees it.
	PowerState func(context.Context) (string, error)
	// Remote administration reuses the owned VM, disk, and append-only serial log.
	ReconnectSerial func(context.Context) error
	StartRetained   func(context.Context) error
	EvidenceDir     string
	HostRunner      commandRunner
	HostPTYRunner   commandRunner
	OpenTerminal    terminalFactory
	logf            func(string, ...any)
}

type Scenario interface {
	Name() string
	Run(ctx context.Context, s *Session) (Result, error)
}

var scenarios = map[string]Scenario{
	"smoke":        smokeScenario{},
	"lscpu":        lscpuScenario{},
	"smp":          smpScenario{},
	"linux":        linuxScenario{},
	"7zip":         sevenZipScenario{},
	"network":      networkScenario{},
	"remote-admin": remoteAdminScenario{},
}

func scenarioNames() string {
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// smokeScenario: the shell runs a native command, and poweroff switches the
// VM off through ACPI.
type smokeScenario struct{}

func (smokeScenario) Name() string { return "smoke" }

func (smokeScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	out, err := g.Run(ctx, "uname", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "uname", err)
	}
	if err := g.Expect("uname", out, "GoOS"); err != nil {
		return "", err
	}
	if s.KeepRunning {
		return ResultPass, nil
	}
	if err := g.con.WriteLine("poweroff"); err != nil {
		return "", failf(ClassInfra, "console", "writing poweroff: %v", err)
	}
	offCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := g.con.ReadUntil(offCtx, []byte("Powering off")); err != nil {
		return "", g.readFailure(ClassScenario, "poweroff", "Powering off", err)
	}
	for {
		state, err := s.PowerState(offCtx)
		if err != nil {
			return "", fail(ClassInfra, "poweroff", err)
		}
		if state == "stopped" {
			g.pass("acpi-poweroff")
			return ResultPass, nil
		}
		select {
		case <-offCtx.Done():
			return "", failf(ClassScenario, "poweroff", "the VM still %s 30s after poweroff", state)
		case <-time.After(time.Second):
		}
	}
}

// lscpuScenario checks that the native command reports the VM topology, with
// every processor online, consistently in every supported output mode.
type lscpuScenario struct{}

func (lscpuScenario) Name() string { return "lscpu" }

func (lscpuScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	sockets, cores := s.Report.VM.Sockets, s.Report.VM.Cores
	if sockets < 1 || cores < 1 {
		return "", failf(ClassConfig, "lscpu", "invalid VM topology: sockets=%d cores=%d", sockets, cores)
	}
	cpus := sockets * cores
	want := map[string]string{
		"Architecture:":        "x86_64",
		"CPU(s):":              strconv.Itoa(cpus),
		"On-line CPU(s) list:": cpuRange(0, cpus-1),
		"Thread(s) per core:":  "1",
		"Core(s) per socket:":  strconv.Itoa(cores),
		"Socket(s):":           strconv.Itoa(sockets),
	}

	summary, err := g.Run(ctx, "lscpu", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "lscpu-summary", err)
	}
	if err := g.Clean("lscpu-summary", summary); err != nil {
		return "", err
	}
	if err := expectLscpuFields(g, "lscpu-summary", summaryFields(summary), want); err != nil {
		return "", err
	}

	extended, err := g.Run(ctx, "lscpu -e=CPU,ONLINE,SOCKET,CORE,THREAD,APICID", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "lscpu-extended", err)
	}
	if err := g.Clean("lscpu-extended", extended); err != nil {
		return "", err
	}
	if err := checkLscpuRows(g, extended, cpus); err != nil {
		return "", err
	}

	jsonOut, err := g.Run(ctx, "lscpu -J", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "lscpu-json", err)
	}
	if err := g.Clean("lscpu-json", jsonOut); err != nil {
		return "", err
	}
	fields, err := jsonLscpuFields(jsonOut)
	if err != nil {
		return "", failf(ClassScenario, "lscpu-json", "invalid JSON: %v", err)
	}
	if err := expectLscpuFields(g, "lscpu-json", fields, want); err != nil {
		return "", err
	}
	return ResultPass, nil
}

func cpuRange(first, last int) string {
	if first == last {
		return strconv.Itoa(first)
	}
	return fmt.Sprintf("%d-%d", first, last)
}

func summaryFields(out string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			fields[strings.TrimSpace(name)+":"] = strings.TrimSpace(value)
		}
	}
	return fields
}

func expectLscpuFields(g *Guest, check string, got, want map[string]string) error {
	for name, value := range want {
		if got[name] != value {
			return failf(ClassScenario, check, "%s = %q, want %q", name, got[name], value)
		}
	}
	g.pass(check)
	return nil
}

func checkLscpuRows(g *Guest, out string, cpus int) error {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	wantHeader := []string{"CPU", "ONLINE", "SOCKET", "CORE", "THREAD", "APICID"}
	if len(lines) == 0 || strings.Join(strings.Fields(lines[0]), " ") != strings.Join(wantHeader, " ") {
		return failf(ClassScenario, "lscpu-extended", "unexpected header:\n%s", out)
	}
	if len(lines)-1 != cpus {
		return failf(ClassScenario, "lscpu-extended", "got %d CPU rows, want %d:\n%s", len(lines)-1, cpus, out)
	}
	for id, line := range lines[1:] {
		cells := strings.Fields(line)
		if len(cells) != len(wantHeader) || cells[0] != strconv.Itoa(id) {
			return failf(ClassScenario, "lscpu-extended", "invalid CPU %d row %q", id, line)
		}
		if cells[1] != "yes" {
			return failf(ClassScenario, "lscpu-extended", "CPU %d ONLINE = %q, want \"yes\"", id, cells[1])
		}
	}
	g.pass("lscpu-extended")
	return nil
}

func jsonLscpuFields(out string) (map[string]string, error) {
	var document struct {
		LSCPU []struct {
			Field string `json:"field"`
			Data  string `json:"data"`
		} `json:"lscpu"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, err
	}
	fields := make(map[string]string, len(document.LSCPU))
	for _, field := range document.LSCPU {
		fields[field.Field] = field.Data
	}
	return fields, nil
}

// linuxScenario runs the committed static Linux programs under KVM.
type linuxScenario struct{}

func (linuxScenario) Name() string { return "linux" }

func (linuxScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	probes := []struct {
		check, command string
		want           []string
	}{
		{"linux-minimal", "/system/linux/hello", []string{"hello from Linux ABI"}},
		{"linux-static-pie", "/system/linux/hello-pie", []string{"hello from a static PIE"}},
		{"linux-musl", "/system/linux/musl-hello alpha beta", []string{"argc=3", "hello from musl"}},
		{"linux-fpstate", "/system/linux/abi-probe fpstate", []string{"fpstate ok"}},
		{"linux-hostile", "/system/linux/abi-probe hostile", []string{"hostile ok"}},
	}
	for _, p := range probes {
		out, err := g.Run(ctx, p.command, commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, p.check, err)
		}
		for _, want := range p.want {
			if err := g.Expect(p.check, out, want); err != nil {
				return "", err
			}
		}
		if err := g.Clean(p.check, out); err != nil {
			return "", err
		}
		if strings.Contains(out, "failed") {
			return "", failf(ClassScenario, p.check, "output reports a failure:\n%s", out)
		}
	}
	return ResultPass, nil
}

// smpScenario runs the Linux probe's smp mode on real virtualization: the
// workers must run on every online processor, overlap in time on distinct
// processors, and survive fork, exec, contended locks, and page discards.
type smpScenario struct{}

func (smpScenario) Name() string { return "smp" }

func (smpScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	cpus := s.Report.VM.Sockets * s.Report.VM.Cores
	out, err := g.Run(ctx, "/system/linux/abi-probe smp", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "smp", err)
	}
	for _, want := range []string{fmt.Sprintf("smp cpus: %d online", cpus), "smp workers:", "smp futex:", "smp vm:", "smp exec:", "smp ok"} {
		if err := g.Expect("smp", out, want); err != nil {
			return "", err
		}
	}
	if err := g.Clean("smp", out); err != nil {
		return "", err
	}
	g.pass("smp")
	return ResultPass, nil
}

// sevenZipScenario runs a static 7zzs the caller placed in /system/linux
// through a round trip: list the codecs, pack a file, test the archive,
// unpack it, and hash the copy beside the original. It records how far it
// got and which Linux calls the kernel refused. It is a diagnostic: unless
// RequirePass is set, a workload that does not complete is the expected
// result.
type sevenZipScenario struct{}

// sevenZipInput is the file the round trip packs: 7-Zip itself, a few MiB
// that keep the multithreaded LZMA2 encoder busy.
const sevenZipInput = "/system/linux/7zzs"

// sevenZipSteps run in order; the first that fails ends the workload. 7-Zip
// chooses its thread count itself, so the add step runs the multithreaded
// encoder.
var sevenZipSteps = []struct{ name, command, want string }{
	{"info", "/system/linux/7zzs i", "Formats:"},
	{"add", "/system/linux/7zzs a /tmp/pvelab.7z " + sevenZipInput, "Everything is Ok"},
	{"test", "/system/linux/7zzs t /tmp/pvelab.7z", "Everything is Ok"},
	{"extract", "/system/linux/7zzs x -y -o/tmp/pvelab-out /tmp/pvelab.7z", "Everything is Ok"},
	{"hash", "/system/linux/7zzs h -scrcSHA256 /tmp/pvelab-out/7zzs " + sevenZipInput, "Everything is Ok"},
}

func (sevenZipScenario) Name() string { return "7zip" }

func (sevenZipScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	listing, err := g.Run(ctx, "ls /system/linux", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "7zip", err)
	}
	if !hasLine(listing, "7zzs") {
		return "", failf(ClassConfig, "7zip", "/system/linux/7zzs is missing; build the disk with LINUX_EXTRA=/path/to/7zzs")
	}

	w := &Workload{}
	s.Report.Workload = w
	for _, step := range sevenZipSteps {
		w.Command = step.command
		out, ending, err := runWorkloadStep(ctx, g, step.command, s.WorkloadTimeout)
		if err != nil {
			w.Ending = ending
			return "", err
		}
		w.Faults = faultLines(out)
		ok := strings.Contains(out, step.want) && len(w.Faults) == 0
		if ok && step.name == "hash" && !sameHashes(out, 2) {
			ok, ending = false, "the unpacked copy differs from the original"
		}
		if !ok {
			if ending == "" {
				ending = workloadEnding(out, false)
			}
			w.Ending = step.name + ": " + ending
			break
		}
		w.Steps = append(w.Steps, step.name)
		g.pass("7zip-" + step.name)
	}
	w.Completed = len(w.Steps) == len(sevenZipSteps)
	if w.Completed {
		w.Ending = "exit 0"
	}

	diag, err := captureDiag(ctx, g)
	if err != nil {
		return "", err
	}
	s.Report.Linux = diag

	if w.Completed {
		return ResultPass, nil
	}
	if s.RequirePass {
		return "", failf(ClassScenario, "7zip", "the workload did not complete (%s)", w.Ending)
	}
	return ResultExpectedFailure, nil
}

// runWorkloadStep runs one command of a workload. A command that holds the
// prompt past timeout gets Ctrl-C; ending then says so. err is set only
// when the shell itself stops answering.
func runWorkloadStep(ctx context.Context, g *Guest, command string, timeout time.Duration) (string, string, error) {
	out, err := g.Run(ctx, command, timeout)
	var late *promptTimeoutError
	switch {
	case errors.As(err, &late):
		if writeErr := g.con.Write([]byte{3}); writeErr != nil {
			return "", "", failf(ClassInfra, "console", "sending Ctrl-C: %v", writeErr)
		}
		rest, err := g.awaitPrompt(ctx, "", 20*time.Second)
		if err != nil {
			return "", "hung: no prompt after Ctrl-C", failf(ClassScenario, "7zip", "the shell did not come back after Ctrl-C:\n%s", tailText(g.con))
		}
		return out + rest, fmt.Sprintf("no prompt within %s; interrupted", timeout), nil
	case err != nil:
		return "", "", fail(ClassScenario, "7zip", err)
	}
	return out, "", nil
}

// sameHashes reports whether 7-Zip's hash listing has count file lines,
// all with one SHA-256.
func sameHashes(out string, count int) bool {
	var hashes []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && len(fields[0]) == 64 && strings.Trim(fields[0], "0123456789abcdef") == "" {
			hashes = append(hashes, fields[0])
		}
	}
	if len(hashes) != count {
		return false
	}
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			return false
		}
	}
	return true
}

func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// faultLines are the lines that say how a process ended badly.
func faultLines(out string) []string {
	var faults []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{userFault, kernelPanic, "fatal", "killed", "exit status:", "signal"} {
			if strings.Contains(line, marker) {
				faults = append(faults, line)
				break
			}
		}
	}
	return faults
}

func workloadEnding(out string, completed bool) string {
	for _, line := range strings.Split(out, "\n") {
		if status, ok := strings.CutPrefix(strings.TrimSpace(line), "exit status:"); ok {
			return "exit status " + strings.TrimSpace(status)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, userFault) || strings.Contains(line, "killed") {
			return strings.TrimSpace(line)
		}
	}
	if completed {
		return "exit 0"
	}
	return "exited without the full listing"
}

// captureDiag runs the shell's diag command and parses its DIAG linux line.
// Only a DIAG=1 build has the command.
func captureDiag(ctx context.Context, g *Guest) (*DiagLinux, error) {
	if err := g.con.WriteLine("diag"); err != nil {
		return nil, failf(ClassInfra, "console", "writing diag: %v", err)
	}
	diagCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	which, out, err := g.con.ReadUntilAny(diagCtx, []byte("DIAG end"), []byte("diag: unavailable"))
	if err != nil {
		return nil, g.readFailure(ClassScenario, "diag", "DIAG end", err)
	}
	if _, err := g.awaitPrompt(ctx, "", 30*time.Second); err != nil {
		return nil, fail(ClassScenario, "diag", err)
	}
	if which == 1 {
		return nil, failf(ClassConfig, "diag", "this build has no diag command; build the inputs with DIAG=1")
	}
	line, ok := FindDiagLinux(string(out))
	if !ok {
		return nil, failf(ClassScenario, "diag", "diag printed no DIAG linux line")
	}
	d, err := ParseDiagLinux(line)
	if err != nil {
		return nil, fail(ClassScenario, "diag", err)
	}
	g.pass("diag-linux")
	return d, nil
}

// networkScenario takes an address from the lab network's DHCP server through
// the GoOS driver and network service, then pings the gateway that server
// named. A LAN may drop a probe now and then, so one reply out of four is
// enough; every request must still be answered or reported.
type networkScenario struct{}

// networkReadyTimeout bounds the wait for a lease on the lab bridge.
const networkReadyTimeout = 45 * time.Second

func (networkScenario) Name() string { return "network" }

func (networkScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	if s.Report.VM.NetBridge == "" {
		return "", failf(ClassInfra, "network", "PVE_NET_BRIDGE is empty, so the VM has no network function")
	}
	deadline := time.Now().Add(networkReadyTimeout)
	var status string
	for {
		out, err := g.Run(ctx, "net status", commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, "net status", err)
		}
		status = out
		if strings.Contains(out, "state: ready") {
			break
		}
		if time.Now().After(deadline) {
			return "", failf(ClassScenario, "dhcp", "no address within %s; net status:\n%s", networkReadyTimeout, out)
		}
		select {
		case <-ctx.Done():
			return "", fail(ClassInfra, "dhcp", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	g.pass("dhcp")
	fields := statusFields(status)
	for key, want := range map[string]string{"link": "up", "mode": "dhcp"} {
		if fields[key] != want {
			return "", failf(ClassScenario, "net status", "%s is %q, want %q:\n%s", key, fields[key], want, status)
		}
	}
	gateway := fields["gateway"]
	if gateway == "" || gateway == "none" {
		return "", failf(ClassScenario, "gateway", "the DHCP server named no router:\n%s", status)
	}
	s.logf("address %s, gateway %s", fields["address"], gateway)
	out, err := g.Run(ctx, "ping -c 4 -W 1000 "+gateway, commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "ping", err)
	}
	if err := g.Clean("ping", out); err != nil {
		return "", err
	}
	sent, received, ok := pingSummary(out)
	if !ok || sent != 4 || received == 0 {
		return "", failf(ClassScenario, "ping", "no reply from %s:\n%s", gateway, out)
	}
	if err := g.Expect("ping", out, "bytes from "+gateway+": seq="); err != nil {
		return "", err
	}
	services, err := g.Run(ctx, "services", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "services", err)
	}
	for _, name := range []string{"netd", "virtio-net", "resolved"} {
		state, restarts, ok := serviceState(services, name)
		if !ok || state != "running" || restarts != 0 {
			return "", failf(ClassScenario, "network stability", "%s state=%q restarts=%d; want running with no restarts:\n%s", name, state, restarts, services)
		}
	}
	g.pass("network-services-stable")
	s.logf("ping %s: %d of %d replies", gateway, received, sent)
	return ResultPass, nil
}

// serviceState reads one row from the services builtin.
func serviceState(out, name string) (string, int, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != name {
			continue
		}
		restarts, err := strconv.Atoi(fields[3])
		return fields[1], restarts, err == nil
	}
	return "", 0, false
}

// statusFields reads the "name: value" lines net status prints.
func statusFields(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields
}

// pingSummary reads ping's "N sent, M received, L% lost" line.
func pingSummary(out string) (int, int, bool) {
	for _, line := range strings.Split(out, "\n") {
		var sent, received, lost int
		if n, _ := fmt.Sscanf(strings.TrimSpace(line), "%d sent, %d received, %d%% lost", &sent, &received, &lost); n == 3 {
			return sent, received, true
		}
	}
	return 0, 0, false
}
