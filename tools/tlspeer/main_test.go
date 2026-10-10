package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"goos/tools/tlsfixture"
)

func TestValidPeerServesFragmentedHTTP(t *testing.T) {
	address, counters, stop := startPeer(t, "valid", 3)
	defer stop()
	client := tlsClient(t, "tls.test")
	conn, err := tls.Dial("tcp", address, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /hello HTTP/1.1\r\nHost: tls.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if got := requestCount(counters, "valid"); got != 1 {
		t.Fatalf("valid requests = %d, want 1", got)
	}
}

func TestHeadAndRedirectResponses(t *testing.T) {
	address, _, stop := startPeer(t, "valid", 7)
	defer stop()
	for _, check := range []struct {
		method, path, location string
		size                   int64
	}{
		{http.MethodHead, "/httphead", "", 5},
		{http.MethodGet, "/redirect", "/hello", 0},
		{http.MethodGet, "/downgrade", "http://tls.test/hello", 0},
	} {
		conn, err := tls.Dial("tcp", address, tlsClient(t, "tls.test"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: tls.test\r\n\r\n", check.method, check.path)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: check.method})
		if err != nil {
			t.Fatal(err)
		}
		if response.ContentLength != check.size || response.Header.Get("Location") != check.location {
			t.Errorf("%s: length=%d location=%q", check.path, response.ContentLength, response.Header.Get("Location"))
		}
		response.Body.Close()
		conn.Close()
	}
}

func TestNegativeFixtureNeverServesHTTP(t *testing.T) {
	address, counters, stop := startPeer(t, "wronghost", 5)
	defer stop()
	_, err := tls.Dial("tcp", address, tlsClient(t, "tls.test"))
	if err == nil {
		t.Fatal("wrong-host TLS handshake succeeded")
	}
	if got := requestCount(counters, "wronghost"); got != 0 {
		t.Fatalf("wronghost requests = %d, want 0", got)
	}
}

func TestPlainHTTPAndMalformedModes(t *testing.T) {
	address, counters, stop := startPeer(t, "plainhttp", 4)
	defer stop()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(conn, "GET /hello HTTP/1.1\r\nHost: tls.test\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	conn.Close()
	if got := requestCount(counters, "plainhttp"); got != 1 {
		t.Fatalf("plain requests = %d, want 1", got)
	}

	address, _, stop = startPeer(t, "malformed", 4)
	defer stop()
	conn, err = net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "not a TLS record") {
		t.Fatalf("malformed data = %q", data)
	}
}

func startPeer(t *testing.T, mode string, fragment int) (string, *peerCounters, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counters := &peerCounters{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, peerConfig{mode: mode, fragment: fragment}, counters)
		}
	}()
	return listener.Addr().String(), counters, func() { listener.Close(); <-done }
}

func tlsClient(t *testing.T, name string) *tls.Config {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(tlsfixture.RootPEM()) {
		t.Fatal("fixture root did not parse")
	}
	return &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}
}

func requestCount(counters *peerCounters, mode string) int {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	return counters.requests[mode]
}

func TestBadVerifyNeverServesHTTP(t *testing.T) {
	address, counters, stop := startPeer(t, "badverify", 5)
	defer stop()
	conn, err := tls.Dial("tcp", address, tlsClient(t, "tls.test"))
	if err == nil {
		conn.Close()
		t.Fatal("bad CertificateVerify handshake succeeded")
	}
	if got := requestCount(counters, "badverify"); got != 0 {
		t.Fatalf("badverify requests = %d, want 0", got)
	}
}

func TestTamperCorruptsFirstHTTPResponseRecord(t *testing.T) {
	address, counters, stop := startPeer(t, "tamper", 5)
	defer stop()
	conn, err := tls.Dial("tcp", address, tlsClient(t, "tls.test"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /hello HTTP/1.1\r\nHost: tls.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_, err = http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err == nil {
		t.Fatal("tampered HTTP response authenticated unexpectedly")
	}
	if got := requestCount(counters, "tamper"); got != 1 {
		t.Fatalf("tamper requests = %d, want 1", got)
	}
}

func TestPlainHTTPRejectsTLSClientHelloWithoutLoggingRequest(t *testing.T) {
	address, counters, stop := startPeer(t, "plainhttp", 5)
	defer stop()
	conn, err := tls.Dial("tcp", address, tlsClient(t, "tls.test"))
	if err == nil {
		conn.Close()
		t.Fatal("TLS client accepted plain HTTP response")
	}
	if got := requestCount(counters, "plainhttp"); got != 0 {
		t.Fatalf("plain HTTP logged TLS ClientHello as request count %d", got)
	}
}

func TestNegativeModeServesOnlyWhenVerificationIsBypassed(t *testing.T) {
	address, counters, stop := startPeer(t, "wronghost", 5)
	defer stop()
	conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}) // #nosec G402 -- host-only negative-fixture test
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /hello HTTP/1.1\r\nHost: ignored.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := requestCount(counters, "wronghost"); got != 1 {
		t.Fatalf("wronghost requests = %d, want 1", got)
	}
}
