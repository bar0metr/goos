package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// Console is the guest's COM1. Scenarios see only this; how Proxmox exposes
// the port stays in the constructor.
type Console interface {
	// ReadUntil returns the output from the read position through the first
	// needle and moves the position past it.
	ReadUntil(ctx context.Context, needle []byte) ([]byte, error)
	// ReadUntilAny is ReadUntil for several needles; it reports which one
	// came first.
	ReadUntilAny(ctx context.Context, needles ...[]byte) (int, []byte, error)
	// Drain moves the read position to the end of what has arrived.
	Drain() []byte
	// Tail is the last n bytes received, for error messages.
	Tail(n int) []byte
	Write([]byte) error
	WriteLine(string) error
	Close() error
}

// errConsoleClosed is returned once the transport has ended.
var errConsoleClosed = errors.New("console closed")

// streamConsole is a Console over any byte stream. Every byte received is
// appended to the log as it arrives.
type streamConsole struct {
	in    io.WriteCloser
	close func() error

	mu      sync.Mutex
	buf     []byte
	pos     int
	arrived chan struct{} // closed and replaced whenever bytes arrive
	ended   error
	log     io.Writer
	// why describes the end of the transport, such as ssh's error output.
	why func() string
}

func newStreamConsole(out io.Reader, in io.WriteCloser, log io.Writer, closeFn func() error) *streamConsole {
	c := &streamConsole{in: in, close: closeFn, arrived: make(chan struct{}), log: log}
	go c.pump(out)
	return c
}

func (c *streamConsole) pump(out io.Reader) {
	chunk := make([]byte, 4096)
	for {
		n, err := out.Read(chunk)
		c.mu.Lock()
		if n > 0 {
			if c.log != nil {
				c.log.Write(chunk[:n])
			}
			c.buf = append(c.buf, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errConsoleClosed
			}
			c.ended = err
		}
		close(c.arrived)
		c.arrived = make(chan struct{})
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *streamConsole) ReadUntil(ctx context.Context, needle []byte) ([]byte, error) {
	_, out, err := c.ReadUntilAny(ctx, needle)
	return out, err
}

func (c *streamConsole) ReadUntilAny(ctx context.Context, needles ...[]byte) (int, []byte, error) {
	for {
		c.mu.Lock()
		pending := c.buf[c.pos:]
		best, bestEnd := -1, -1
		for i, needle := range needles {
			if at := bytes.Index(pending, needle); at >= 0 && (best < 0 || at+len(needle) < bestEnd) {
				best, bestEnd = i, at+len(needle)
			}
		}
		if best >= 0 {
			out := append([]byte(nil), pending[:bestEnd]...)
			c.pos += bestEnd
			c.mu.Unlock()
			return best, out, nil
		}
		ended, arrived := c.ended, c.arrived
		c.mu.Unlock()
		if ended != nil {
			if c.why != nil {
				if why := c.why(); why != "" {
					return -1, nil, fmt.Errorf("%w: %s", ended, why)
				}
			}
			return -1, nil, ended
		}
		select {
		case <-ctx.Done():
			return -1, nil, ctx.Err()
		case <-arrived:
		}
	}
}

func (c *streamConsole) Drain() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]byte(nil), c.buf[c.pos:]...)
	c.pos = len(c.buf)
	return out
}

func (c *streamConsole) Tail(n int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(c.buf) - n
	if start < 0 {
		start = 0
	}
	return append([]byte(nil), c.buf[start:]...)
}

func (c *streamConsole) Write(b []byte) error {
	_, err := c.in.Write(b)
	return err
}

// WriteLine sends a command line. GoOS's serial input takes \n as Enter.
func (c *streamConsole) WriteLine(s string) error { return c.Write([]byte(s + "\n")) }

func (c *streamConsole) Close() error {
	c.in.Close()
	if c.close != nil {
		return c.close()
	}
	return nil
}

// AttachSerial connects to the VM's serial0 socket through SSH and socat.
// It may be called before the VM starts: the remote side waits for QEMU to
// create the socket, so the console sees the boot from its first line.
// Closing the console ends socat and leaves the VM running.
func AttachSerial(ssh SSH, vmid int, logPath string) (Console, error) {
	socket := "/var/run/qemu-server/" + strconv.Itoa(vmid) + ".serial0"
	script := `s=` + shellQuote(socket) + `; i=0; while [ ! -S "$s" ]; do i=$((i+1)); [ "$i" -gt 1200 ] && { echo "no serial socket $s" >&2; exit 3; }; sleep 0.1; done; exec socat - UNIX-CONNECT:"$s"`
	argv := ssh.Root("sh", "-c", script)
	// The console outlives any one stage, so it has its own lifetime;
	// Close ends it.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := ssh.Command(ctx, shellJoin(argv...))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		log.Close()
		return nil, fmt.Errorf("starting ssh: %w", err)
	}
	var once sync.Once
	closeFn := func() error {
		var err error
		once.Do(func() {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				<-done
			}
			cancel()
			log.Close()
		})
		return err
	}
	c := newStreamConsole(stdout, stdin, log, closeFn)
	c.why = func() string { return lastLines(stderr.String(), 4) }
	return c, nil
}

// lockedBuffer is a bytes.Buffer safe to read while exec writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
