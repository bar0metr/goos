//go:build goos_userspace

package remoteservice

import (
	"errors"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/remoteapi"
	"goos/userspace/runtime/sys"
	"io"
	"net"
	"time"
)

func Now() uint64 { return remoteapi.Now() }

var Receive = remoteapi.Receive
var Send = remoteapi.Send
var CloseHandles = remoteapi.CloseHandles

// RelayConn is a worker's private ciphertext stream pair, never a listener.
// All waits have absolute deadlines and no goroutines are required.
type RelayConn struct {
	Input, Output         handle.ID
	readUntil, writeUntil uint64
	closed                bool
	TimeoutTicks          uint64
}

var errDeadline = errors.New("remote transport deadline")

func (c *RelayConn) wait(id handle.ID, event kcall.WaitEvent, deadline uint64) error {
	if deadline == 0 {
		deadline = Now() + c.TimeoutTicks
	}
	now := Now()
	if now >= deadline {
		return errDeadline
	}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: id, Events: event | kcall.WaitPeerClosed}
	_, status := sys.WaitAny(&interests, 1, deadline-now)
	if status == kcall.StatusTimeout {
		return errDeadline
	}
	if status != kcall.StatusOK {
		return io.ErrClosedPipe
	}
	return nil
}
func (c *RelayConn) Read(b []byte) (int, error) {
	deadline := c.readUntil
	if deadline == 0 {
		deadline = Now() + c.TimeoutTicks
	}
	for {
		if c.closed {
			return 0, io.ErrClosedPipe
		}
		if len(b) == 0 {
			return 0, nil
		}
		n, status := sys.StreamRead(c.Input, b[:min(len(b), int(kcall.MaxStreamIO))])
		if status == kcall.StatusOK && n > 0 {
			return n, nil
		}
		if status == kcall.StatusPeerDied {
			return n, io.EOF
		}
		if status != kcall.StatusResourceExhausted && status != kcall.StatusOK {
			return n, io.ErrClosedPipe
		}
		if err := c.wait(c.Input, kcall.WaitReadable, deadline); err != nil {
			return 0, err
		}
	}
}
func (c *RelayConn) Write(b []byte) (int, error) {
	deadline := c.writeUntil
	if deadline == 0 {
		deadline = Now() + c.TimeoutTicks
	}
	done := 0
	for len(b) > 0 {
		if c.closed {
			return done, io.ErrClosedPipe
		}
		n, status := sys.StreamWrite(c.Output, b[:min(len(b), 256)])
		done += n
		b = b[n:]
		if status == kcall.StatusOK {
			if n == 0 {
				return done, io.ErrNoProgress
			}
			continue
		}
		if status != kcall.StatusResourceExhausted {
			return done, io.ErrClosedPipe
		}
		if err := c.wait(c.Output, kcall.WaitWritable, deadline); err != nil {
			return done, err
		}
	}
	return done, nil
}
func (c *RelayConn) Close() error {
	if !c.closed {
		c.closed = true
		sys.HandleClose(c.Input)
		sys.HandleClose(c.Output)
	}
	return nil
}

type relayAddress struct{}

func (relayAddress) Network() string      { return "native-stream" }
func (relayAddress) String() string       { return "private-worker" }
func (c *RelayConn) LocalAddr() net.Addr  { return relayAddress{} }
func (c *RelayConn) RemoteAddr() net.Addr { return relayAddress{} }
func tickDeadline(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	delta := time.Until(t)
	if delta <= 0 {
		return Now()
	}
	return Now() + uint64(delta)*clock.TicksPerSecond/1_000_000_000 + 1
}
func (c *RelayConn) SetDeadline(t time.Time) error {
	c.readUntil = tickDeadline(t)
	c.writeUntil = c.readUntil
	return nil
}
func (c *RelayConn) SetReadDeadline(t time.Time) error  { c.readUntil = tickDeadline(t); return nil }
func (c *RelayConn) SetWriteDeadline(t time.Time) error { c.writeUntil = tickDeadline(t); return nil }
func (c *RelayConn) HandshakeDeadline(ticks uint64) {
	c.readUntil = Now() + ticks
	c.writeUntil = c.readUntil
}
func (c *RelayConn) ClearDeadline() { c.readUntil, c.writeUntil = 0, 0 }
