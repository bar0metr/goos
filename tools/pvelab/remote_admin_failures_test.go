package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemoteAdminRebootMustOccur(t *testing.T) {
	m := newScenarioMock(t)
	m.noReboot = true
	result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || result == ResultPass || !strings.Contains(err.Error(), "boot") {
		t.Fatalf("accepted shutdown without reboot: %s %v", result, err)
	}
}

func TestRemoteAdminWidthMustChange(t *testing.T) {
	m := newScenarioMock(t)
	m.terminal.fixedWidth = true
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || !strings.Contains(err.Error(), "column") {
		t.Fatalf("width ignored: %v", err)
	}
}

func TestRemoteAdminPrivateDisclosureRedacted(t *testing.T) {
	m := newScenarioMock(t)
	m.privateLeak = "-----BEGIN OPENSSH PRIVATE KEY-----\nSENSITIVE-HOST-PRIVATE-BYTES\n"
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || !strings.Contains(err.Error(), "host-private-key-denied") {
		t.Fatalf("disclosure accepted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(m.s.EvidenceDir, "remote-admin.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range [][]byte{data, jsonIndent(m.s.Report)} {
		if bytes.Contains(evidence, []byte("SENSITIVE-HOST-PRIVATE-BYTES")) || bytes.Contains(evidence, []byte("BEGIN OPENSSH PRIVATE")) {
			t.Fatal("private key leaked into report evidence")
		}
	}
}

func TestRemoteAdminFollowDelayedCursor(t *testing.T) {
	m := newScenarioMock(t)
	m.dropFollowEvents = 1
	result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err != nil || result != ResultPass || m.generation != 3 {
		t.Fatalf("retry failed: %s %v generation=%d", result, err, m.generation)
	}
}

func TestRemoteAdminFollowDoesNotAcceptOldGeneration(t *testing.T) {
	m := newScenarioMock(t)
	m.dropFollowEvents = 3
	result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || result == ResultPass || !strings.Contains(err.Error(), "generation-tagged") {
		t.Fatalf("old event accepted: %s %v", result, err)
	}
}

func TestRemoteAdminTextUtilitiesRejectExtraLines(t *testing.T) {
	for _, command := range []string{"head -n 1", "tail -n 1", "grep beta"} {
		t.Run(command, func(t *testing.T) {
			m := newScenarioMock(t)
			m.s.Guest.con.(*scriptConsole).reply = func(line string) string {
				if strings.HasPrefix(line, command) {
					return "alpha\nbeta\ngamma"
				}
				return m.serialReply(line)
			}
			if result, err := (remoteAdminScenario{}).Run(context.Background(), m.s); err == nil || result == ResultPass {
				t.Fatalf("accepted extra lines: %s %v", result, err)
			}
		})
	}
}

func TestRemoteAdminInvalidEvidenceBlocked(t *testing.T) {
	m := newScenarioMock(t)
	if err := os.WriteFile(filepath.Join(m.s.EvidenceDir, "host-info.json"), []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || classOf(err) != ClassInfra || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("bad evidence accepted: %v", err)
	}
}

type failedEvidenceWriter struct {
	calls int
	short bool
}

func (w *failedEvidenceWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(data), nil
	}
	if w.short {
		return len(data) - 1, nil
	}
	return 0, errors.New("evidence storage full")
}

func TestConsoleEvidenceErrorsCannotHideBehindPrompt(t *testing.T) {
	for _, short := range []bool{false, true} {
		data := strings.Repeat("x", 4096) + shellPrompt
		c := newStreamConsole(strings.NewReader(data), nopWriteCloser{}, &failedEvidenceWriter{short: short}, nil)
		<-c.pumpDone
		if _, err := c.ReadUntil(context.Background(), []byte(shellPrompt)); err == nil {
			t.Fatal("prompt accepted despite lost evidence")
		}
		if err := c.Close(); err == nil {
			t.Fatal("close hid evidence failure")
		}
	}
}

func TestConsoleOverflowAfterMarkerFailsClose(t *testing.T) {
	c := newBoundedStreamConsole(strings.NewReader(shellPrompt+strings.Repeat("x", 128)), nopWriteCloser{}, nil, nil, 64)
	<-c.pumpDone
	if _, err := c.ReadUntil(context.Background(), []byte(shellPrompt)); err == nil {
		t.Fatal("bound hidden by marker")
	}
	if err := c.Close(); err == nil {
		t.Fatal("close hid bound failure")
	}
}

func TestInteractiveStderrBound(t *testing.T) {
	canceled := false
	output := &limitedOutput{onError: func() { canceled = true }}
	if _, err := output.Write(bytes.Repeat([]byte{'x'}, guestOutputLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("extra")); err == nil || !canceled {
		t.Fatal("stderr overflow did not stop terminal")
	}
	text, err := output.result()
	if err == nil || len(text) != guestOutputLimit {
		t.Fatalf("overflow hidden len=%d err=%v", len(text), err)
	}
}

type delayedEvidenceWriter struct {
	bytes.Buffer
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *delayedEvidenceWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.release })
	return w.Buffer.Write(data)
}
func (w *delayedEvidenceWriter) Close() error { return nil }

func TestSerialTailChild(t *testing.T) {
	if os.Getenv("PVELAB_SERIAL_HELPER") != "1" {
		return
	}
	os.Stdout.Write(bytes.Repeat([]byte{'x'}, 8192))
	os.Stdout.WriteString("FINAL-SERIAL-TAIL\n")
	os.Exit(0)
}

func TestSerialCloseDrainsSlowEvidence(t *testing.T) {
	t.Setenv("PVELAB_SERIAL_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log := &delayedEvidenceWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-log.release:
		default:
			close(log.release)
		}
	}()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSerialTailChild$")
	console, err := startSerialConsole(cmd, cancel, log)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-log.started:
	case <-ctx.Done():
		t.Fatal("serial child produced no output")
	}
	closed := make(chan error, 1)
	go func() { closed <- console.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("closed before evidence drain: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(log.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serial close did not finish")
	}
	want := strings.Repeat("x", 8192) + "FINAL-SERIAL-TAIL\n"
	if log.String() != want {
		t.Fatalf("lost serial tail: got %d bytes, want %d", log.Len(), len(want))
	}
}

func TestOutputBoundChild(t *testing.T) {
	if os.Getenv("PVELAB_OUTPUT_HELPER") != "1" {
		return
	}
	os.Stdout.Write(bytes.Repeat([]byte{'x'}, guestOutputLimit+1))
	os.Exit(7)
}

func TestRunHostCommandOverflowNonzero(t *testing.T) {
	t.Setenv("PVELAB_OUTPUT_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := runHostCommand(ctx, os.Args[0], "-test.run=^TestOutputBoundChild$")
	if err == nil || out.Status != -1 || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("nonzero status hid overflow: %+v %v", out, err)
	}
}
