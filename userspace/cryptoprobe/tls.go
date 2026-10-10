//go:build cryptoprobe_tls

package cryptoprobe

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

const Name = "tls"

type tlsUnsupported struct{ reason string }

func (e tlsUnsupported) Error() string {
	return "TLS unsupported/offload required: " + e.reason
}

func Run() error {
	reader := tlsProbeReader()
	previous := rand.Reader
	rand.Reader = reader
	defer func() { rand.Reader = previous }()
	wire := &tlsProbeConn{}
	client := tls.Client(wire, &tls.Config{
		Rand:             reader,
		ServerName:       "probe.invalid",
		MinVersion:       tls.VersionTLS13,
		MaxVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
	})
	// EOF is deliberate: emitting ClientHello alone does not prove usable TLS.
	err := client.Handshake()
	return classifyTLSHandshake(wire.data[:wire.used], err)
}

func classifyTLSHandshake(wire []byte, err error) error {
	if len(wire) == 0 {
		if err == nil {
			return tlsUnsupported{reason: "no-op shim returned success without ClientHello"}
		}
		return tlsUnsupported{reason: "handshake failed before ClientHello"}
	}
	if len(wire) < 9 || wire[0] != 22 || wire[1] != 3 || wire[5] != 1 {
		return tlsUnsupported{reason: "handshake wrote no valid ClientHello record"}
	}
	recordLen := int(wire[3])<<8 | int(wire[4])
	if recordLen < 4 || recordLen > len(wire)-5 {
		return tlsUnsupported{reason: "incomplete ClientHello record"}
	}
	if err == nil {
		return tlsUnsupported{reason: "handshake claimed success without a peer transcript"}
	}
	return tlsUnsupported{reason: "ClientHello emitted; peer handshake and trust unverified"}
}

type tlsProbeConn struct {
	data [8192]byte
	used int
}

func (c *tlsProbeConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *tlsProbeConn) Write(p []byte) (int, error) {
	if len(p) > len(c.data)-c.used {
		return 0, errors.New("TLS probe transcript exceeded 8192 bytes")
	}
	n := copy(c.data[c.used:], p)
	c.used += n
	return n, nil
}

func (*tlsProbeConn) Close() error                     { return nil }
func (*tlsProbeConn) LocalAddr() net.Addr              { return tlsProbeAddr{} }
func (*tlsProbeConn) RemoteAddr() net.Addr             { return tlsProbeAddr{} }
func (*tlsProbeConn) SetDeadline(time.Time) error      { return nil }
func (*tlsProbeConn) SetReadDeadline(time.Time) error  { return nil }
func (*tlsProbeConn) SetWriteDeadline(time.Time) error { return nil }

type tlsProbeAddr struct{}

func (tlsProbeAddr) Network() string { return "probe" }
func (tlsProbeAddr) String() string  { return "probe.invalid:443" }
