package tlsclient

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"goos/tools/tlsfixture"
)

var fixtureNow = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

func TestOpenAuthenticatesFixtureDNSAndIP(t *testing.T) {
	for _, check := range []struct {
		name, host, expectedSNI string
	}{
		{"DNS", "tls.test", "tls.test"},
		{"numeric IP", "10.0.2.2", ""},
	} {
		t.Run(check.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverResult := serveFixture(t, server, "valid", check.expectedSNI)
			conn, err := Open(hostStream{client}, hostConfig(t, client, check.host, fixedNow(fixtureNow)))
			if err != nil {
				t.Fatalf("Open: %v; server: %v", err, <-serverResult)
			}
			if conn.Version() != tls.VersionTLS13 {
				t.Fatalf("TLS version = %#x", conn.Version())
			}
			conn.Close()
			if err := <-serverResult; err != nil {
				t.Fatalf("server handshake: %v", err)
			}
		})
	}
}

func TestOpenAuthenticatesOverFragmentedTransport(t *testing.T) {
	client, server := net.Pipe()
	fragmentedClient := &fragmentedConn{Conn: client, max: 7}
	fragmentedServer := &fragmentedConn{Conn: server, max: 7}
	serverResult := serveFixture(t, fragmentedServer, "valid", "tls.test")
	conn, err := Open(hostStream{fragmentedClient}, hostConfig(t, fragmentedClient, "tls.test", fixedNow(fixtureNow)))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := <-serverResult; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestOpenRejectsNegativeFixturesBeforeHTTP(t *testing.T) {
	for _, check := range []struct {
		name, mode, host, want string
	}{
		{"wrong host", "wronghost", "tls.test", "hostname"},
		{"missing numeric SAN", "valid", "10.0.2.99", "hostname"},
		{"expired", "expired", "tls.test", "expired"},
		{"future", "future", "tls.test", "not yet valid"},
		{"untrusted root", "untrusted", "tls.test", "unknown certificate authority"},
		{"invalid issuer signature", "invalidsignature", "tls.test", "unknown certificate authority"},
		{"unknown critical extension", "unknowncriticalextension", "tls.test", "TLS handshake failed"},
	} {
		t.Run(check.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverResult := serveFixture(t, server, check.mode, expectedSNI(check.host))
			conn, err := Open(hostStream{client}, hostConfig(t, client, check.host, fixedNow(fixtureNow)))
			if conn != nil || err == nil {
				t.Fatalf("Open = %v, %v; want authentication failure", conn, err)
			}
			if !strings.Contains(err.Error(), check.want) {
				t.Fatalf("Open error = %q, want %q", err, check.want)
			}
			// A peer can receive only the client alert after an authentication
			// failure. It must not complete an HTTP/application-data exchange.
			if err := <-serverResult; err == nil {
				t.Fatal("server completed TLS handshake after rejected authentication")
			}
		})
	}
}

func TestOpenRejectsPlainMalformedAndTamperedRecords(t *testing.T) {
	for _, check := range []struct {
		name  string
		serve func(net.Conn)
		wrap  func(net.Conn) net.Conn
	}{
		{
			name: "plain HTTP",
			serve: func(conn net.Conn) {
				readClientHello(conn)
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
			},
			wrap: func(conn net.Conn) net.Conn { return conn },
		},
		{
			name: "malformed TLS record",
			serve: func(conn net.Conn) {
				readClientHello(conn)
				_, _ = conn.Write([]byte{0x16, 0x03, 0x04, 0x00, 0x01, 0xff})
			},
			wrap: func(conn net.Conn) net.Conn { return conn },
		},
		{
			name: "tampered authenticated record",
			serve: func(conn net.Conn) {
				certificate, err := tlsfixture.Certificate("valid")
				if err != nil {
					return
				}
				_ = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}).Handshake()
			},
			wrap: func(conn net.Conn) net.Conn { return &tamperReadConn{Conn: conn} },
		},
	} {
		t.Run(check.name, func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); defer server.Close(); check.serve(server) }()
			conn, err := Open(hostStream{check.wrap(client)}, hostConfig(t, client, "tls.test", fixedNow(fixtureNow)))
			if conn != nil || err == nil {
				t.Fatalf("Open = %v, %v; want protocol failure", conn, err)
			}
			<-done
		})
	}
}

func TestOpenRejectsClockAndRandomFailures(t *testing.T) {
	for _, check := range []struct {
		name   string
		now    func() (time.Time, error)
		random io.Reader
		want   string
	}{
		{"unavailable clock", func() (time.Time, error) { return time.Time{}, errors.New("clock unavailable") }, rand.Reader, "clock"},
		{"zero clock", fixedNow(time.Time{}), rand.Reader, "clock"},
		{"rollback clock", sequenceNow(fixtureNow, fixtureNow.Add(-time.Second)), rand.Reader, "rolled back"},
		{"random reader failure", fixedNow(fixtureNow), failingReader{}, "TLS handshake failed"},
	} {
		t.Run(check.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverResult := serveFixture(t, server, "valid", "tls.test")
			config := hostConfig(t, client, "tls.test", check.now)
			config.Random = check.random
			conn, err := Open(hostStream{client}, config)
			if conn != nil || err == nil {
				t.Fatalf("Open = %v, %v; want failure", conn, err)
			}
			if !strings.Contains(err.Error(), check.want) {
				t.Fatalf("Open error = %q, want %q", err, check.want)
			}
			<-serverResult
		})
	}
}

func hostConfig(t *testing.T, conn net.Conn, host string, now func() (time.Time, error)) Config {
	t.Helper()
	roots, err := LoadRoots(tlsfixture.RootPEM())
	if err != nil {
		t.Fatal(err)
	}
	return Config{Host: host, Roots: roots, Random: rand.Reader, Now: now, SetDeadline: conn.SetDeadline}
}

func readClientHello(conn net.Conn) {
	buffer := make([]byte, 4096)
	_, _ = conn.Read(buffer)
}

func expectedSNI(host string) string {
	if net.ParseIP(host) != nil {
		return ""
	}
	return host
}

func fixedNow(value time.Time) func() (time.Time, error) {
	return func() (time.Time, error) { return value, nil }
}

func sequenceNow(values ...time.Time) func() (time.Time, error) {
	var mu sync.Mutex
	index := 0
	return func() (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		if index >= len(values) {
			return values[len(values)-1], nil
		}
		value := values[index]
		index++
		return value, nil
	}
}

type hostStream struct{ net.Conn }

func (s hostStream) Close() { _ = s.Conn.Close() }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("test random source failure") }

// fragmentedConn performs each underlying socket operation in small pieces
// while preserving net.Conn's full-write contract.
type fragmentedConn struct {
	net.Conn
	max int
}

func (c *fragmentedConn) Read(p []byte) (int, error) {
	if len(p) > c.max {
		p = p[:c.max]
	}
	return c.Conn.Read(p)
}

func (c *fragmentedConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > c.max {
			n = c.max
		}
		m, err := c.Conn.Write(p[:n])
		written += m
		p = p[m:]
		if err != nil || m != n {
			if err == nil {
				err = io.ErrShortWrite
			}
			return written, err
		}
	}
	return written, nil
}

type tamperReadConn struct {
	net.Conn
	tampered bool
}

func (c *tamperReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.tampered {
		p[n-1] ^= 0x01
		c.tampered = true
	}
	return n, err
}

func serveFixture(t *testing.T, conn net.Conn, mode, wantSNI string) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	certificate, err := tlsfixture.Certificate(mode)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer conn.Close()
		config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}
		config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if hello.ServerName != wantSNI {
				return nil, errors.New("unexpected SNI: " + hello.ServerName)
			}
			selected := config.Clone()
			selected.GetConfigForClient = nil
			return selected, nil
		}
		server := tls.Server(conn, config)
		err := server.Handshake()
		result <- err
		if err == nil {
			_, _ = io.Copy(io.Discard, server)
		}
	}()
	return result
}
