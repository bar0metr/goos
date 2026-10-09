package tlsclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"time"
)

// Stream is an existing connected byte stream; TLS does not resolve or dial.
type Stream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close()
}

// Config contains application-owned platform adapters, not trust bypasses.
type Config struct {
	Host        string
	Roots       *Roots
	Random      io.Reader
	Now         func() (time.Time, error)
	SetDeadline func(time.Time) error
}

// Conn exposes authenticated application data only after Open succeeds.
type Conn struct{ conn *tls.Conn }

func Open(stream Stream, config Config) (*Conn, error) {
	if stream == nil || config.Host == "" || config.Roots == nil || config.Roots.pool == nil || config.Roots.count == 0 || config.Random == nil || config.Now == nil || config.SetDeadline == nil {
		if stream != nil {
			stream.Close()
		}
		return nil, errors.New("TLS configuration or trust store unavailable")
	}
	first, err := config.Now()
	if err != nil || first.Unix() <= 0 || first.Year() > 9999 {
		stream.Close()
		return nil, errors.New("realtime clock unavailable or invalid")
	}
	last := first
	var clockErr error
	readTime := func() time.Time {
		value, err := config.Now()
		if err != nil || value.Unix() <= 0 || value.Year() > 9999 {
			clockErr = errors.New("realtime clock unavailable or invalid")
			return first
		}
		if value.Before(last) {
			clockErr = errors.New("realtime clock rolled back during TLS validation")
			return first
		}
		last = value
		return value
	}
	verify := func(state tls.ConnectionState) error {
		readTime()
		if clockErr != nil {
			return clockErr
		}
		if state.Version != tls.VersionTLS13 || len(state.PeerCertificates) == 0 || len(state.PeerCertificates) > MaxCertificates || len(state.VerifiedChains) == 0 {
			return errors.New("TLS peer authentication incomplete")
		}
		for _, cert := range state.PeerCertificates {
			if len(cert.Raw) == 0 || len(cert.Raw) > MaxDER || len(cert.DNSNames) > MaxDNSNames {
				return errors.New("certificate exceeds native bounds")
			}
		}
		for _, chain := range state.VerifiedChains {
			if len(chain) > MaxCertificates {
				return errors.New("certificate chain exceeds native depth")
			}
		}
		if state.NegotiatedProtocol != "" && state.NegotiatedProtocol != "http/1.1" {
			return errors.New("TLS peer selected an unsupported application protocol")
		}
		return nil
	}
	transport := &netStream{Stream: stream, deadline: config.SetDeadline}
	if err := transport.SetDeadline(time.Now().Add(35 * time.Second)); err != nil {
		transport.Close()
		return nil, err
	}
	client := tls.Client(transport, &tls.Config{
		RootCAs:                config.Roots.pool,
		ServerName:             config.Host,
		Rand:                   config.Random,
		Time:                   readTime,
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		CurvePreferences:       []tls.CurveID{tls.X25519},
		NextProtos:             []string{"http/1.1"},
		SessionTicketsDisabled: true,
		VerifyConnection:       verify,
	})
	if err := client.Handshake(); err != nil {
		client.Close()
		if clockErr != nil {
			return nil, clockErr
		}
		return nil, certificateError(err, last)
	}
	// Reject a device-offload shim even if its Handshake claims success.
	if err := verify(client.ConnectionState()); err != nil {
		client.Close()
		return nil, err
	}
	if err := transport.SetDeadline(time.Time{}); err != nil {
		client.Close()
		return nil, err
	}
	return &Conn{conn: client}, nil
}

func (c *Conn) Read(p []byte) (int, error)  { return c.conn.Read(p) }
func (c *Conn) Write(p []byte) (int, error) { return c.conn.Write(p) }
func (c *Conn) Close()                      { c.conn.Close() }
func (c *Conn) Version() uint16             { return c.conn.ConnectionState().Version }
func (c *Conn) CipherSuite() uint16         { return c.conn.ConnectionState().CipherSuite }

func certificateError(err error, now time.Time) error {
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &authority) {
		return errors.New("unknown certificate authority")
	}
	if errors.As(err, &hostname) {
		return errors.New("hostname does not match certificate")
	}
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
		if now.Before(invalid.Cert.NotBefore) {
			return errors.New("certificate not yet valid")
		}
		return errors.New("certificate expired")
	}
	return errors.New("TLS handshake failed: " + err.Error())
}

type netStream struct {
	Stream
	deadline func(time.Time) error
}

func (s *netStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := s.Stream.Read(p)
	if n < 0 || n > len(p) {
		return 0, io.ErrNoProgress
	}
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	return n, err
}

func (s *netStream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := s.Stream.Write(p[written:])
		if n < 0 || n > len(p)-written {
			return written, io.ErrShortWrite
		}
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (s *netStream) Close() error       { s.Stream.Close(); return nil }
func (*netStream) LocalAddr() net.Addr  { return streamAddress{} }
func (*netStream) RemoteAddr() net.Addr { return streamAddress{} }
func (s *netStream) SetDeadline(value time.Time) error {
	if s.deadline == nil {
		return errors.New("TLS transport deadline unavailable")
	}
	return s.deadline(value)
}
func (s *netStream) SetReadDeadline(value time.Time) error  { return s.SetDeadline(value) }
func (s *netStream) SetWriteDeadline(value time.Time) error { return s.SetDeadline(value) }

type streamAddress struct{}

func (streamAddress) Network() string { return "tcp" }
func (streamAddress) String() string  { return "native stream" }
