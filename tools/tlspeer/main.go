// Command tlspeer runs a small deterministic host-only HTTPS peer for GoOS
// integration tests. It uses the standard library TLS server deliberately; it
// is not included in a native image.
package main

import (
	"bufio"
	"crypto"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"goos/tools/tlsfixture"
)

type peerConfig struct {
	mode     string
	fragment int
}

type peerCounters struct {
	mu       sync.Mutex
	requests map[string]int
}

func (c *peerCounters) request(mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requests == nil {
		c.requests = make(map[string]int)
	}
	c.requests[mode]++
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tlspeer:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("tlspeer", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8443", "TCP address to listen on")
	mode := flags.String("mode", "valid", "fixture mode")
	fragment := flags.Int("fragment", 137, "maximum host socket read/write fragment")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("no positional arguments are accepted")
	}
	if *fragment <= 0 {
		return fmt.Errorf("-fragment must be positive, got %d", *fragment)
	}
	if !knownMode(*mode) {
		return fmt.Errorf("unknown mode %q", *mode)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	counters := &peerCounters{}
	quit := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					close(quit)
					return
				}
				continue
			}
			go serveConn(conn, peerConfig{mode: *mode, fragment: *fragment}, counters)
		}
	}()
	fmt.Printf("tlspeer: listening %s mode=%s\n", listener.Addr(), *mode)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-signals:
		return listener.Close()
	case <-quit:
		return nil
	}
}

func knownMode(mode string) bool {
	switch mode {
	case "valid", "wronghost", "expired", "future", "notyetvalid", "untrusted", "untrustedroot", "invalidsignature", "unknowncriticalextension", "badverify", "tamper", "malformed", "plainhttp":
		return true
	default:
		return false
	}
}

func serveConn(conn net.Conn, cfg peerConfig, counters *peerCounters) {
	defer conn.Close()
	fragmented := &fragmentConn{Conn: conn, max: cfg.fragment}
	switch cfg.mode {
	case "plainhttp":
		servePlainHTTP(fragmented, cfg.mode, counters)
	case "malformed":
		// A TLS record starts with content type 20-23, so this is unambiguously
		// malformed protocol input rather than an HTTP endpoint.
		_, _ = fragmented.Write([]byte("not a TLS record\n"))
	default:
		fixtureMode := cfg.mode
		if fixtureMode == "badverify" || fixtureMode == "tamper" {
			fixtureMode = "valid"
		}
		certificate, err := tlsfixture.Certificate(fixtureMode)
		if err != nil {
			return
		}
		if cfg.mode == "badverify" {
			signer, ok := certificate.PrivateKey.(crypto.Signer)
			if !ok {
				return
			}
			certificate.PrivateKey = badVerifySigner{Signer: signer}
		}
		var transport net.Conn = fragmented
		var tamper *tamperConn
		if cfg.mode == "tamper" {
			tamper = &tamperConn{fragmentConn: fragmented}
			transport = tamper
		}
		tlsConn := tls.Server(transport, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
		})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		// Certificate-negative modes still serve HTTP after a successful server
		// handshake. This lets host tests prove that rejection came from client
		// verification rather than the peer withholding application data.
		if tamper != nil {
			tamper.armed = true
		}
		serveHTTP(tlsConn, cfg.mode, counters)
	}
}

func servePlainHTTP(conn net.Conn, mode string, counters *peerCounters) {
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		return
	}
	// A TLS ClientHello starts with a record content type (20 through 23).
	// Plain HTTP intentionally answers it as HTTP without treating encrypted
	// bytes as a request or logging a request route.
	if first[0] >= 20 && first[0] <= 23 {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 5\r\nContent-Type: text/plain\r\n\r\nhello")
		return
	}
	serveHTTPReader(conn, reader, mode, counters)
}

func serveHTTP(conn net.Conn, mode string, counters *peerCounters) {
	serveHTTPReader(conn, bufio.NewReader(conn), mode, counters)
}

func serveHTTPReader(conn net.Conn, reader *bufio.Reader, mode string, counters *peerCounters) {
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	defer request.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20)); err != nil {
		return
	}
	status, location, body := reply(request)
	contentLength := len(body)
	if request.Method == http.MethodHead {
		body = nil
	}
	fmt.Fprintf(conn, "HTTP/1.1 %s\r\nConnection: close\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n", status, contentLength)
	if location != "" {
		fmt.Fprintf(conn, "Location: %s\r\n", location)
	}
	fmt.Fprint(conn, "\r\n")
	if len(body) != 0 {
		_, _ = conn.Write(body)
	}
	counters.request(mode)
	fmt.Printf("tlspeer: request mode=%s method=%s path=%s\n", mode, request.Method, request.URL.Path)
}

func reply(request *http.Request) (status, location string, body []byte) {
	switch request.URL.Path {
	case "/hello", "/httphead":
		return "200 OK", "", []byte("hello")
	case "/redirect":
		return "302 Found", "/hello", nil
	case "/downgrade":
		return "302 Found", "http://tls.test/hello", nil
	default:
		return "404 Not Found", "", nil
	}
}

// fragmentConn constrains individual host socket operations without changing
// net.Conn's full-write contract. This makes the TLS record stream arrive in
// small fragments, exercising clients that correctly handle partial I/O.
type fragmentConn struct {
	net.Conn
	max int
}

func (c *fragmentConn) Read(p []byte) (int, error) {
	if len(p) > c.max {
		p = p[:c.max]
	}
	return c.Conn.Read(p)
}

func (c *fragmentConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) != 0 {
		n := len(p)
		if n > c.max {
			n = c.max
		}
		m, err := c.Conn.Write(p[:n])
		written += m
		p = p[m:]
		if err != nil {
			return written, err
		}
		if m != n {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// badVerifySigner corrupts only CertificateVerify. Its certificate remains
// valid, so clients must reject the handshake signature rather than x509 data.
type badVerifySigner struct{ crypto.Signer }

func (s badVerifySigner) Sign(random io.Reader, digest []byte, options crypto.SignerOpts) ([]byte, error) {
	signature, err := s.Signer.Sign(random, digest, options)
	if err == nil && len(signature) != 0 {
		signature[len(signature)-1] ^= 1
	}
	return signature, err
}

// tamperConn corrupts the first encrypted application record only after TLS
// handshake completion. It keeps the ordinary fragmented full-write contract.
type tamperConn struct {
	*fragmentConn
	armed    bool
	tampered bool
}

func (c *tamperConn) Write(data []byte) (int, error) {
	if c.armed && !c.tampered && len(data) != 0 {
		copyData := append([]byte(nil), data...)
		copyData[len(copyData)-1] ^= 1
		c.tampered = true
		return c.fragmentConn.Write(copyData)
	}
	return c.fragmentConn.Write(data)
}
