package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// nanoGuest answers each step's bytes with what the step waits for, then a
// shell prompt for the commands the scenario runs.
func nanoGuest(t *testing.T, stopAt string) *streamConsole {
	t.Helper()
	toGuestR, toGuestW := io.Pipe()
	fromGuestR, fromGuestW := io.Pipe()
	go func() {
		defer fromGuestW.Close()
		line := func(reply string) {
			buffer := make([]byte, 1)
			for {
				if _, err := toGuestR.Read(buffer); err != nil || buffer[0] == '\n' {
					break
				}
			}
			io.WriteString(fromGuestW, "x\r\n"+reply+"\r\n"+shellPrompt)
		}
		line("busybox\nnano\ntty-probe")
		line("tty identity ok")
		for _, step := range nanoSteps {
			if step.name == stopAt {
				// A guest that stopped answering still takes the keys.
				io.Copy(io.Discard, toGuestR)
				return
			}
			if _, err := io.ReadFull(toGuestR, make([]byte, len(step.send))); err != nil {
				return
			}
			io.WriteString(fromGuestW, step.want)
		}
		line("from nano first line")
		line(sevenZipDiag)
	}()
	c := newStreamConsole(fromGuestR, toGuestW, &strings.Builder{}, func() error { toGuestR.Close(); return nil })
	t.Cleanup(func() { c.Close() })
	return c
}

func TestNanoScenarioPassesEveryStep(t *testing.T) {
	saved := nanoKeyDelay
	nanoKeyDelay = 0
	defer func() { nanoKeyDelay = saved }()
	s, report := newSession(nanoGuest(t, ""))
	s.WorkloadTimeout = 5 * time.Second
	result, err := nanoScenario{}.Run(context.Background(), s)
	if err != nil || result != ResultPass || report.Linux == nil {
		t.Fatalf("%s %v %+v", result, err, report.Linux)
	}
}

func TestNanoScenarioFailsAtTheStepThatHangs(t *testing.T) {
	saved := nanoKeyDelay
	nanoKeyDelay = 0
	defer func() { nanoKeyDelay = saved }()
	s, _ := newSession(nanoGuest(t, "nano-saves"))
	s.WorkloadTimeout = 200 * time.Millisecond
	if _, err := (nanoScenario{}).Run(context.Background(), s); err == nil || classOf(err) != ClassScenario {
		t.Fatalf("hung step: %v", err)
	}
}
