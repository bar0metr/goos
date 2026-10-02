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

func TestSevenZipExpectedFailure(t *testing.T) {
	replies := map[string]string{
		"ls /system/linux": "7zzs\nabi-probe",
		sevenZipCommand:    "7-Zip (z) 24.08 (x64)\nuser fault: rip=0x4a1b2c addr=0x0\nexit status: 139",
		"diag":             sevenZipDiag,
	}
	con, _ := fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, report := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	result, err := sevenZipScenario{}.Run(context.Background(), s)
	if err != nil || result != ResultExpectedFailure {
		t.Fatalf("%s %v", result, err)
	}
	if report.Workload.Completed || report.Workload.Ending != "exit status 139" || len(report.Workload.Faults) != 2 {
		t.Errorf("workload %+v", report.Workload)
	}
	if report.Linux == nil || report.Linux.Unsupported != 7 {
		t.Errorf("linux %+v", report.Linux)
	}

	// --require-pass turns the same run into a workload failure.
	con, _ = fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, _ = newSession(con)
	s.RequirePass = true
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	if _, err := (sevenZipScenario{}).Run(context.Background(), s); err == nil || classOf(err) != ClassScenario {
		t.Fatalf("require-pass: %v", err)
	}
}

func TestSevenZipHangIsInterrupted(t *testing.T) {
	replies := map[string]string{"ls /system/linux": "7zzs", "diag": sevenZipDiag}
	con, _ := fakeGuest{boot: normalBoot, replies: replies, hang: map[string]bool{sevenZipCommand: true}}.start(t)
	s, report := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	result, err := sevenZipScenario{}.Run(context.Background(), s)
	if err != nil || result != ResultExpectedFailure || !strings.Contains(report.Workload.Ending, "interrupted") {
		t.Fatalf("%s %v %+v", result, err, report.Workload)
	}
}

func TestSevenZipPass(t *testing.T) {
	replies := map[string]string{
		"ls /system/linux": "7zzs",
		sevenZipCommand:    "7-Zip (z) 24.08 (x64)\n\nFormats:\n 7z\n\nCodecs:\n LZMA",
		"diag":             sevenZipDiag,
	}
	con, _ := fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, _ := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	if result, err := (sevenZipScenario{}).Run(context.Background(), s); err != nil || result != ResultPass {
		t.Fatalf("%s %v", result, err)
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
	replies := map[string]string{"ls /system/linux": "7zzs", sevenZipCommand: "exit status: 1", "diag": "diag: unavailable in this build"}
	con, _ := fakeGuest{boot: normalBoot, replies: replies}.start(t)
	s, _ := newSession(con)
	s.Guest.WaitBoot(context.Background(), 5*time.Second)
	if _, err := (sevenZipScenario{}).Run(context.Background(), s); classOf(err) != ClassConfig {
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
