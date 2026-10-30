package main

import (
	"context"
	"time"
)

// nanoScenario drives Linux programs on their terminal over the serial
// console of a KVM guest (ADR 0206): the repository's tty-probe, an
// interactive BusyBox sh with Ctrl-C and job control, and GNU nano editing,
// saving, suspending, and reopening a file. The caller places busybox and
// nano in /system/linux; the repository carries neither.
type nanoScenario struct{}

const nanoFile = "/persist/pvelab-nano.txt"

// nanoKeyDelay spaces keystrokes. The terminal service reports Ctrl-C ahead
// of bytes still queued, so keys go out as a person types them.
var nanoKeyDelay = 300 * time.Millisecond

// nanoStep sends keys, waits settle, and reads the console until want. An
// empty send only reads, and an empty want only sends.
type nanoStep struct {
	name, send, want string
	settle           time.Duration
}

var nanoSteps = []nanoStep{
	{name: "shell-prompt", send: "/system/linux/busybox sh\n", want: "$ "},
	{name: "shell-command", send: "echo sum=$((40+2))\n", want: "sum=42"},
	{name: "foreground-child", send: "/system/linux/busybox sleep 30\n", settle: 2 * time.Second},
	{name: "ctrl-c-echo", send: "\x03", want: "^C"},
	{name: "ctrl-c-status", send: "echo status=$?\n", want: "status=130"},
	{name: "nano-draws", send: "printf 'first line\\n' > " + nanoFile + "; /system/linux/nano -I " + nanoFile + "\n", want: "GNU nano"},
	{name: "nano-shows-file", want: "first line"},
	{name: "nano-types", send: "from nano ", want: "from nano"},
	{name: "nano-asks-name", send: "\x0f", want: "Write to File:"},
	{name: "nano-saves", send: "\r", want: "Wrote 1 line"},
	{name: "nano-exits", send: "\x18", want: "$ "},
	{name: "saved-bytes", send: "/system/linux/busybox cat " + nanoFile + "\n", want: "\nfrom nano first line"},
	{name: "nano-reopens", send: "/system/linux/nano -I " + nanoFile + "\n", want: "from nano first line"},
	{name: "suspend-prefix", send: "\x14"},
	{name: "nano-suspends", send: "\x1a", want: "Stopped"},
	{name: "shell-has-terminal", send: "echo back=$((6*7))\n", want: "back=42"},
	{name: "fg-redraws", send: "fg\n", want: "from nano first line"},
	{name: "nano-exits-after-fg", send: "\x18", want: "$ "},
	{name: "shell-exits", send: "exit\n", want: shellPrompt},
	{name: "native-nano", send: "/system/linux/nano -I " + nanoFile + "\n", want: "from nano first line"},
	{name: "native-nano-exits", send: "\x18", want: shellPrompt},
}

func (nanoScenario) Name() string { return "nano" }

func (nanoScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	listing, err := g.Run(ctx, "ls /system/linux", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "nano", err)
	}
	for _, program := range []string{"busybox", "nano"} {
		if !hasLine(listing, program) {
			return "", failf(ClassConfig, "nano", "/system/linux/%s is missing; build with make test-pve-nano", program)
		}
	}
	out, err := g.Run(ctx, "/system/linux/tty-probe identity", s.WorkloadTimeout)
	if err != nil {
		return "", fail(ClassScenario, "tty-probe", err)
	}
	if err := g.Expect("tty-probe", out, "tty identity ok"); err != nil {
		return "", err
	}
	for _, step := range nanoSteps {
		if err := g.terminalStep(ctx, step, s.WorkloadTimeout); err != nil {
			return "", err
		}
	}
	// The native shell reads what nano saved.
	out, err = g.Run(ctx, "cat "+nanoFile, commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "native-cat", err)
	}
	if err := g.Expect("native-cat", out, "from nano first line"); err != nil {
		return "", err
	}
	diag, err := captureDiag(ctx, g)
	if err != nil {
		return "", err
	}
	s.Report.Linux = diag
	return ResultPass, nil
}

// terminalStep runs one step and records its check.
func (g *Guest) terminalStep(ctx context.Context, step nanoStep, timeout time.Duration) error {
	check := "nano-" + step.name
	if step.send != "" {
		if err := g.con.Write([]byte(step.send)); err != nil {
			return failf(ClassInfra, "console", "writing %s: %v", check, err)
		}
		select {
		case <-time.After(nanoKeyDelay + step.settle):
		case <-ctx.Done():
			return failf(ClassInfra, check, "interrupted")
		}
	}
	if step.want != "" {
		waiting, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		which, _, err := g.con.ReadUntilAny(waiting, []byte(step.want), []byte(kernelPanic))
		if err != nil {
			return g.readFailure(ClassScenario, check, step.want, err)
		}
		if which == 1 {
			return failf(ClassBoot, check, "kernel panic:\n%s", tailText(g.con))
		}
	}
	g.pass(check)
	return nil
}
