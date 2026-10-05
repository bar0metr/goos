package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeGuest answers command lines the way the GoOS shell does over COM1:
// it echoes the line, prints the reply, and prints the prompt.
type fakeGuest struct {
	boot    string
	replies map[string]string
	hang    map[string]bool
}

func (f fakeGuest) start(t *testing.T) (*streamConsole, *bytes.Buffer) {
	t.Helper()
	toGuestR, toGuestW := io.Pipe()
	fromGuestR, fromGuestW := io.Pipe()
	log := &bytes.Buffer{}
	go func() {
		defer fromGuestW.Close()
		io.WriteString(fromGuestW, f.boot)
		lines := bufio.NewReader(toGuestR)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			command := strings.TrimRight(line, "\n")
			io.WriteString(fromGuestW, command+"\r\n")
			if f.hang[command] {
				// Wait for Ctrl-C, then give the prompt back.
				for {
					b, err := lines.ReadByte()
					if err != nil {
						return
					}
					if b == 3 {
						io.WriteString(fromGuestW, "^C\r\n\r\n"+shellPrompt)
						break
					}
				}
				continue
			}
			io.WriteString(fromGuestW, strings.ReplaceAll(f.replies[command], "\n", "\r\n")+"\r\n"+shellPrompt)
		}
	}()
	c := newStreamConsole(fromGuestR, toGuestW, log, func() error { toGuestR.Close(); return nil })
	t.Cleanup(func() { c.Close() })
	return c, log
}

const normalBoot = "GoOS 0.2.2\r\n[ OK ] vfs\r\n[MODE] normal\r\n[ OK ] shell\r\n\r\n" + shellPrompt

func newSession(c Console) (*Session, *Report) {
	report := &Report{}
	return &Session{
		Guest: &Guest{con: c, logf: func(string, ...any) {}}, Report: report, WorkloadTimeout: 300 * time.Millisecond,
		PowerState: func(context.Context) (string, error) { return "stopped", nil },
	}, report
}

func TestReadUntilAndLog(t *testing.T) {
	c, log := fakeGuest{boot: "one\r\ntwo\r\nthree"}.start(t)
	out, err := c.ReadUntil(context.Background(), []byte("two"))
	if err != nil || string(out) != "one\r\ntwo" {
		t.Fatalf("%q %v", out, err)
	}
	which, out, err := c.ReadUntilAny(context.Background(), []byte("nope"), []byte("three"))
	if err != nil || which != 1 || string(out) != "\r\nthree" {
		t.Fatalf("%d %q %v", which, out, err)
	}
	if !strings.Contains(log.String(), "one\r\ntwo\r\nthree") {
		t.Errorf("log %q", log.String())
	}
}

func TestReadUntilTimeout(t *testing.T) {
	c, _ := fakeGuest{boot: "booting"}.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.ReadUntil(ctx, []byte("never")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// The data is still there for the next reader.
	if out, err := c.ReadUntil(context.Background(), []byte("booting")); err != nil || string(out) != "booting" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestReadUntilEndOfStream(t *testing.T) {
	r, w := io.Pipe()
	c := newStreamConsole(r, nopWriteCloser{}, nil, nil)
	io.WriteString(w, "partial")
	w.Close()
	if _, err := c.ReadUntil(context.Background(), []byte("never")); !errors.Is(err, errConsoleClosed) {
		t.Fatalf("err = %v", err)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func TestWaitBootClassifies(t *testing.T) {
	cases := map[string]struct {
		boot  string
		class Class
	}{
		"recovery": {"[MODE] recovery: stage=mount\r\n" + shellPrompt, ClassBoot},
		"panic":    {"GoOS\r\nPANIC: page fault\r\n", ClassBoot},
		"silent":   {"GoOS 0.2.2\r\n", ClassBoot},
	}
	for name, c := range cases {
		con, _ := fakeGuest{boot: c.boot}.start(t)
		s, _ := newSession(con)
		err := s.Guest.WaitBoot(context.Background(), 200*time.Millisecond)
		if err == nil || classOf(err) != c.class {
			t.Errorf("%s: err = %v (class %s)", name, err, classOf(err))
		}
	}
	con, _ := fakeGuest{boot: normalBoot}.start(t)
	s, _ := newSession(con)
	if err := s.Guest.WaitBoot(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeScenario(t *testing.T) {
	con, _ := fakeGuest{boot: normalBoot, replies: map[string]string{"uname": "GoOS 0.2.2 x86_64", "poweroff": "Powering off..."}}.start(t)
	s, _ := newSession(con)
	if err := s.Guest.WaitBoot(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := smokeScenario{}.Run(context.Background(), s)
	if err != nil || result != ResultPass {
		t.Fatalf("%s %v", result, err)
	}
}

func lscpuReplies() map[string]string {
	return map[string]string{
		"lscpu": "Architecture:             x86_64\nCPU(s):                   4\nOn-line CPU(s) list:      0-3\nThread(s) per core:       1\nCore(s) per socket:       2\nSocket(s):                2",
		"lscpu -e=CPU,ONLINE,SOCKET,CORE,THREAD,APICID": "CPU ONLINE SOCKET CORE THREAD APICID\n0 yes 0 0 0 0\n1 yes 0 1 0 1\n2 yes 1 0 0 2\n3 yes 1 1 0 3",
		"lscpu -J": `{"lscpu":[{"field":"Architecture:","data":"x86_64"},{"field":"CPU(s):","data":"4"},{"field":"On-line CPU(s) list:","data":"0-3"},{"field":"Thread(s) per core:","data":"1"},{"field":"Core(s) per socket:","data":"2"},{"field":"Socket(s):","data":"2"}]}`,
	}
}

func runLscpu(t *testing.T, replies map[string]string) (Result, error) {
	t.Helper()
	con, _ := fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, report := newSession(con)
	report.VM.Sockets, report.VM.Cores = 2, 2
	if err := s.Guest.WaitBoot(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return lscpuScenario{}.Run(context.Background(), s)
}

func TestLscpuScenario(t *testing.T) {
	result, err := runLscpu(t, lscpuReplies())
	if err != nil || result != ResultPass {
		t.Fatalf("%s %v", result, err)
	}
}

func TestLscpuScenarioRejectsWrongCPUCount(t *testing.T) {
	replies := lscpuReplies()
	replies["lscpu"] = strings.Replace(replies["lscpu"], "CPU(s):                   4", "CPU(s):                   3", 1)
	_, err := runLscpu(t, replies)
	if err == nil || classOf(err) != ClassScenario || !strings.Contains(err.Error(), `CPU(s): = "3", want "4"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestCleanOutput(t *testing.T) {
	if got := cleanOutput("uname", "uname\r\nGoOS 0.2.2\r\n"+shellPrompt); got != "GoOS 0.2.2" {
		t.Errorf("%q", got)
	}
}

func TestLinuxScenarioFailsOnBadOutput(t *testing.T) {
	replies := map[string]string{
		"/system/linux/hello":                 "hello from Linux ABI",
		"/system/linux/hello-pie":             "hello from a static PIE",
		"/system/linux/musl-hello alpha beta": "argc=3\nhello from musl",
		"/system/linux/abi-probe fpstate":     "user fault: rip=0x401000\nexit status: 139",
		"/system/linux/abi-probe hostile":     "hostile ok",
	}
	con, _ := fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, _ := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	_, err := linuxScenario{}.Run(context.Background(), s)
	if err == nil || classOf(err) != ClassScenario || !strings.Contains(err.Error(), "linux-fpstate") {
		t.Fatalf("err = %v", err)
	}
}

const sevenZipDiag = "DIAG waits ipc=0\n" + recordedDiag + "\nDIAG end"

const sevenZipHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// sevenZipReplies answers every round-trip step with success.
func sevenZipReplies() map[string]string {
	replies := map[string]string{"ls /system/linux": "7zzs\nabi-probe", "diag": sevenZipDiag}
	for _, step := range sevenZipSteps {
		replies[step.command] = "7-Zip (z) 26.03 (x64)\n\nEverything is Ok"
	}
	replies[sevenZipSteps[0].command] = "7-Zip (z) 26.03 (x64)\n\nFormats:\n 7z\n\nCodecs:\n LZMA"
	replies[sevenZipSteps[4].command] = sevenZipHash + "  2557064  7zzs\n" + sevenZipHash + "  2557064  7zzs\n\nEverything is Ok"
	return replies
}

func runSevenZip(t *testing.T, replies map[string]string, hang map[string]bool, requirePass bool) (Result, *Report, error) {
	t.Helper()
	con, _ := fakeGuest{boot: normalBoot, replies: replies, hang: hang}.start(t)
	s, report := newSession(con)
	s.RequirePass = requirePass
	if err := s.Guest.WaitBoot(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := sevenZipScenario{}.Run(context.Background(), s)
	return result, report, err
}

func TestSevenZipRoundTripPasses(t *testing.T) {
	result, report, err := runSevenZip(t, sevenZipReplies(), nil, false)
	if err != nil || result != ResultPass || !report.Workload.Completed || len(report.Workload.Steps) != len(sevenZipSteps) {
		t.Fatalf("%s %v %+v", result, err, report.Workload)
	}
	if report.Linux == nil || report.Linux.Unsupported != 7 {
		t.Errorf("linux %+v", report.Linux)
	}
}

func TestSevenZipExpectedFailure(t *testing.T) {
	replies := sevenZipReplies()
	replies[sevenZipSteps[0].command] = ": error while loading shared libraries: cannot apply additional memory protection after relocation: Permission denied\nexit status: 127"
	result, report, err := runSevenZip(t, replies, nil, false)
	if err != nil || result != ResultExpectedFailure {
		t.Fatalf("%s %v", result, err)
	}
	w := report.Workload
	if w.Completed || w.Ending != "info: exit status 127" || len(w.Steps) != 0 || w.Command != sevenZipSteps[0].command {
		t.Errorf("workload %+v", w)
	}
	if report.Linux == nil {
		t.Error("no DIAG linux after a failed step")
	}
	// --require-pass turns the same run into a workload failure.
	if _, _, err := runSevenZip(t, replies, nil, true); err == nil || classOf(err) != ClassScenario {
		t.Fatalf("require-pass: %v", err)
	}
}

func TestSevenZipStopsAtTheFailingStep(t *testing.T) {
	replies := sevenZipReplies()
	replies[sevenZipSteps[1].command] = "Creating archive: /tmp/pvelab.7z\n\nSystem ERROR:\nerrno=22 : Invalid argument\nexit status: 2"
	result, report, err := runSevenZip(t, replies, nil, false)
	w := report.Workload
	if err != nil || result != ResultExpectedFailure || strings.Join(w.Steps, ",") != "info" || w.Ending != "add: exit status 2" {
		t.Fatalf("%s %v %+v", result, err, w)
	}
}

func TestSevenZipDetectsADifferentCopy(t *testing.T) {
	replies := sevenZipReplies()
	other := strings.Repeat("0", 64)
	replies[sevenZipSteps[4].command] = sevenZipHash + "  2557064  7zzs\n" + other + "  2557064  7zzs\n\nEverything is Ok"
	result, report, err := runSevenZip(t, replies, nil, false)
	if err != nil || result != ResultExpectedFailure || !strings.Contains(report.Workload.Ending, "differs") {
		t.Fatalf("%s %v %+v", result, err, report.Workload)
	}
}

func TestSevenZipHangIsInterrupted(t *testing.T) {
	result, report, err := runSevenZip(t, sevenZipReplies(), map[string]bool{sevenZipSteps[0].command: true}, false)
	if err != nil || result != ResultExpectedFailure || !strings.Contains(report.Workload.Ending, "interrupted") {
		t.Fatalf("%s %v %+v", result, err, report.Workload)
	}
}

func TestSevenZipMissingIsConfig(t *testing.T) {
	con, _ := fakeGuest{boot: normalBoot, replies: map[string]string{"ls /system/linux": "abi-probe"}}.start(t)
	s, _ := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	if _, err := (sevenZipScenario{}).Run(context.Background(), s); classOf(err) != ClassConfig {
		t.Fatalf("err = %v", err)
	}
}

func TestSevenZipWithoutDiagIsConfig(t *testing.T) {
	replies := sevenZipReplies()
	replies["diag"] = "diag: unavailable in this build"
	if _, _, err := runSevenZip(t, replies, nil, false); classOf(err) != ClassConfig {
		t.Fatalf("err = %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	for class, want := range map[Class]int{ClassConfig: 2, ClassInfra: 3, ClassCleanup: 4, ClassBoot: 1, ClassScenario: 1} {
		if got := exitCode(class); got != want {
			t.Errorf("%s: %d, want %d", class, got, want)
		}
	}
}
