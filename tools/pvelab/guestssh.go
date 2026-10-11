package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const guestOutputLimit = 2 << 20

type commandResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Status int    `json:"exit_status"`
}

type commandRunner func(context.Context, string, ...string) (commandResult, error)

// limitedOutput bounds even a malfunctioning guest's output. Exceeding the
// bound fails the check rather than silently validating a truncated reply.
type limitedOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	err     error
	onError func()
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > guestOutputLimit-b.buf.Len() {
		b.err = errors.New("guest output exceeds 2 MiB")
		if b.onError != nil {
			b.onError()
		}
		return 0, b.err
	}
	return b.buf.Write(p)
}

func (b *limitedOutput) result() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.err
}

func runHostCommand(ctx context.Context, name string, args ...string) (commandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr limitedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	outText, outErr := stdout.result()
	errText, errErr := stderr.result()
	result := commandResult{Stdout: outText, Stderr: errText}
	if outputErr := errors.Join(outErr, errErr); outputErr != nil {
		result.Status = -1
		return result, outputErr
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			result.Status = exit.ExitCode()
			return result, nil
		}
		result.Status = -1
		return result, err
	}
	return result, nil
}

// guestSSH is separate from node SSH: no user configuration, agent identity,
// or global known_hosts can weaken the run's serial-verified host-key pin.
type guestSSH struct {
	Host, User, Key, KnownHosts string
	Port                        int
	run                         commandRunner
}

func (s guestSSH) args(pty bool, command string) []string {
	mode := "-T"
	if pty {
		mode = "-tt"
	}
	args := []string{
		"-F", "/dev/null", mode,
		"-p", strconv.Itoa(s.Port), "-l", s.User, "-i", s.Key,
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + s.KnownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=10", "-o", "ConnectionAttempts=1",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3",
		"-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PubkeyAcceptedAlgorithms=ssh-ed25519",
		"-o", "KexAlgorithms=curve25519-sha256", "-o", "Ciphers=aes128-gcm@openssh.com",
		"-o", "SendEnv=-*", s.Host,
	}
	if command != "" {
		args = append(args, command)
	}
	return args
}

func (s guestSSH) exec(ctx context.Context, command string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.run(ctx, "ssh", s.args(false, command)...)
}

func makeClientKey(dir, name string) (path, public, fingerprint string, err error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	encoded, err := ssh.MarshalPrivateKey(private, "pvelab-"+name)
	if err != nil {
		return "", "", "", err
	}
	path = filepath.Join(dir, name)
	if err = os.WriteFile(path, pem.EncodeToMemory(encoded), 0o600); err != nil {
		return "", "", "", err
	}
	key, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return "", "", "", err
	}
	public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " pvelab-" + name
	return path, public, ssh.FingerprintSHA256(key), nil
}

// pinHostKey accepts only one scanned Ed25519 identity, matched to the full
// SHA256 fingerprint obtained over trusted serial before any authentication.
func pinHostKey(scan, trusted, host string, port int) ([]byte, error) {
	var found ssh.PublicKey
	for _, line := range strings.Split(scan, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "ssh-ed25519" || found != nil {
			return nil, errors.New("keyscan must return exactly one Ed25519 host key")
		}
		key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(fields[1] + " " + fields[2]))
		if err != nil || len(rest) != 0 || key.Type() != "ssh-ed25519" {
			return nil, errors.New("invalid scanned host key")
		}
		found = key
	}
	if found == nil || ssh.FingerprintSHA256(found) != trusted {
		return nil, errors.New("scanned host key does not match trusted serial SHA256 fingerprint")
	}
	address := host
	if port != 22 {
		address = "[" + host + "]:" + strconv.Itoa(port)
	}
	return []byte(address + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(found))) + "\n"), nil
}

type remoteTerminal interface {
	Console
	Resize(columns, rows int) error
	Wait(context.Context) (commandResult, error)
}

type terminalFactory func(context.Context, guestSSH, string) (remoteTerminal, error)

type hostTerminal struct {
	*streamConsole
	master    *os.File
	cmd       *exec.Cmd
	stderr    *limitedOutput
	cancel    context.CancelFunc
	done      chan struct{}
	result    commandResult
	err       error
	closeOnce sync.Once
}

// The PTY exists only on the macOS/Linux client. The guest still receives
// native SSH terminal requests, not a POSIX PTY or runtime.
func openGuestTerminal(ctx context.Context, s guestSSH, logPath string) (remoteTerminal, error) {
	master, slave, err := openHostPTY()
	if err != nil {
		return nil, err
	}
	if err := rawHostPTY(slave); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	terminalCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(terminalCtx, "ssh", s.args(true, "")...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin, cmd.Stdout = slave, slave
	cmd.Env = append(os.Environ(), "TERM=xterm")
	stderr := &limitedOutput{onError: cancel}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		master.Close()
		slave.Close()
		log.Close()
		return nil, err
	}
	slave.Close()
	t := &hostTerminal{master: master, cmd: cmd, stderr: stderr, cancel: cancel, done: make(chan struct{})}
	t.streamConsole = newBoundedStreamConsole(master, master, log, nil, guestOutputLimit)
	go func() {
		t.err = cmd.Wait()
		text, stderrErr := stderr.result()
		t.err = errors.Join(t.err, stderrErr)
		t.result = commandResult{Stderr: text}
		if cmd.ProcessState != nil {
			t.result.Status = cmd.ProcessState.ExitCode()
		} else {
			t.result.Status = -1
		}
		close(t.done)
	}()
	return t, nil
}

func (t *hostTerminal) Resize(columns, rows int) error {
	if columns < 1 || columns > 1000 || rows < 1 || rows > 1000 {
		return errors.New("terminal geometry outside 1..1000")
	}
	if err := unix.IoctlSetWinsize(int(t.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(columns)}); err != nil {
		return err
	}
	return t.cmd.Process.Signal(syscall.SIGWINCH)
}

func (t *hostTerminal) Wait(ctx context.Context) (commandResult, error) {
	select {
	case <-ctx.Done():
		return commandResult{Status: -1}, ctx.Err()
	case <-t.done:
		select {
		case <-ctx.Done():
			return t.result, ctx.Err()
		case <-t.streamConsole.pumpDone:
		}
		if err := t.streamConsole.evidenceError(); err != nil {
			return t.result, err
		}
		if t.result.Status != 0 {
			return t.result, fmt.Errorf("SSH terminal exited %d: %s", t.result.Status, t.result.Stderr)
		}
		return t.result, t.err
	}
}

func (t *hostTerminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		select {
		case <-t.done:
		default:
			if killErr := t.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				err = killErr
			}
		}
		t.cancel()
		t.master.Close()
		<-t.done
		<-t.streamConsole.pumpDone
		_, stderrErr := t.stderr.result()
		err = errors.Join(err, stderrErr, t.streamConsole.evidenceError())
		if closer, ok := t.streamConsole.log.(*os.File); ok {
			if closeErr := closer.Close(); err == nil {
				err = closeErr
			}
		}
	})
	return err
}
