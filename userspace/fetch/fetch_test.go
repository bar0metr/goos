package fetch

import (
	"bytes"
	"goos/userspace/network"
	"io"
	"testing"
)

func TestURLAndHeader(t *testing.T) {
	var u URL
	for _, raw := range []string{"ftp://host/", "http://a@host/", "http://[::1]/", "http://host:0/", "http://bad..host/", "http://1.2.3.999/", "http://host/\r\nX: y"} {
		if ParseURL([]byte(raw), &u) == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if ParseURL([]byte("http://Example.test:8080?x=1#f"), &u) != nil || string(u.HostBytes()) != "example.test" || u.Port != 8080 || string(u.TargetBytes()) != "/?x=1" {
		t.Fatalf("URL: %+v", u)
	}
	var h Header
	for _, raw := range []string{"X-Test: good\r\nInjected: bad", "Content-Length: 4", "Transfer-Encoding: chunked", "Connection: keep-alive", "Bad Name: value"} {
		if ParseHeader([]byte(raw), &h) == nil {
			t.Errorf("accepted header %q", raw)
		}
	}
}

type mockConn struct {
	read     []byte
	written  []byte
	maxWrite int
	closed   bool
}

func (c *mockConn) Read(p []byte) (int, error) {
	if len(c.read) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.read)
	c.read = c.read[n:]
	return n, nil
}
func (c *mockConn) Write(p []byte) (int, error) {
	n := len(p)
	if c.maxWrite > 0 && n > c.maxWrite {
		n = c.maxWrite
	}
	c.written = append(c.written, p[:n]...)
	return n, nil
}
func (c *mockConn) Close() { c.closed = true }

type mockOutput struct {
	bytes.Buffer
	closed bool
}

func (o *mockOutput) Write(p []byte) error { _, err := o.Buffer.Write(p); return err }
func (o *mockOutput) Close() error         { o.closed = true; return nil }

type mockEnv struct {
	conns     []*mockConn
	stdout    mockOutput
	file      mockOutput
	path      []byte
	resolved  int
	connected int
	failFirst bool
}

func (e *mockEnv) Resolve(_ []byte, a *[8]network.IPv4) (int, error) {
	e.resolved++
	a[0] = network.IPv4{192, 0, 2, 53}
	if e.failFirst {
		a[1] = network.IPv4{192, 0, 2, 54}
		return 2, nil
	}
	return 1, nil
}
func (e *mockEnv) Connect(_ network.IPv4, _ uint16) (Connection, error) {
	if e.failFirst && e.connected == 0 {
		e.connected++
		return nil, ErrRequest
	}
	c := e.conns[e.connected]
	e.connected++
	return c, nil
}
func (e *mockEnv) Open(p []byte) (Output, error) {
	e.path = append([]byte(nil), p...)
	return &e.file, nil
}
func (e *mockEnv) Stdout() Output    { return &e.stdout }
func (e *mockEnv) Diagnostic([]byte) {}

func TestRequestAndRedirect(t *testing.T) {
	var o Options
	args := [][]byte{[]byte("-H"), []byte("X-Test: one"), []byte("-H"), []byte("X-Test: two"), []byte("-d"), []byte("a=b"), []byte("-L"), []byte("-O"), []byte("http://http.test/post")}
	if err := Parse(args, &o); err != nil {
		t.Fatal(err)
	}
	e := &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 302 Found\r\nLocation: /hello\r\nContent-Length: 0\r\n\r\n"), maxWrite: 3}, {read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"), maxWrite: 3}}}
	if err := Run(&o, e); err != nil {
		t.Fatal(err)
	}
	if e.file.String() != "hello" || string(e.path) != "hello" || !e.file.closed || !e.conns[0].closed || !e.conns[1].closed {
		t.Fatalf("output: %+v", e)
	}
	if !bytes.HasPrefix(e.conns[0].written, []byte("POST /post HTTP/1.1\r\n")) || !bytes.Contains(e.conns[0].written, []byte("Content-Length: 3\r\n")) || !bytes.Contains(e.conns[0].written, []byte("X-Test: one\r\nX-Test: two\r\n")) {
		t.Fatalf("post %q", e.conns[0].written)
	}
	if !bytes.HasPrefix(e.conns[1].written, []byte("GET /hello HTTP/1.1\r\n")) || bytes.Contains(e.conns[1].written, []byte("Content-Length:")) {
		t.Fatalf("redirect %q", e.conns[1].written)
	}
}

func TestHeadAndInclude(t *testing.T) {
	var o Options
	if err := Parse([][]byte{[]byte("-I"), []byte("-i"), []byte("http://192.0.2.53/hello")}, &o); err != nil {
		t.Fatal(err)
	}
	e := &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")}}}
	if err := Run(&o, e); err != nil {
		t.Fatal(err)
	}
	if e.resolved != 0 || e.stdout.String() != "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n" {
		t.Fatalf("head output %q", e.stdout.String())
	}
}

func TestRedirectLimitAndHeaders(t *testing.T) {
	var o Options
	if err := Parse([][]byte{[]byte("-L"), []byte("-i"), []byte("http://http.test/start")}, &o); err != nil {
		t.Fatal(err)
	}
	var conns []*mockConn
	for i := 0; i <= MaxRedirects; i++ {
		conns = append(conns, &mockConn{read: []byte("HTTP/1.1 307 Temporary Redirect\r\nLocation: /again\r\nContent-Length: 0\r\n\r\n")})
	}
	e := &mockEnv{conns: conns}
	if err := Run(&o, e); err != ErrRedirect || e.stdout.Len() != 0 {
		t.Fatalf("limit: %v output %q", err, e.stdout.String())
	}
	for _, c := range conns {
		if !c.closed {
			t.Fatal("open redirect connection")
		}
	}
	var invalid Options
	for _, args := range [][][]byte{{[]byte("-I"), []byte("-d"), []byte("x"), []byte("http://h/")}, {[]byte("-o"), []byte("x"), []byte("-O"), []byte("http://h/")}, {[]byte("ftp://h/")}} {
		if Parse(args, &invalid) == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestIncludedResponse(t *testing.T) {
	var o Options
	if err := Parse([][]byte{[]byte("-i"), []byte("-o"), []byte("saved"), []byte("http://http.test/hello")}, &o); err != nil {
		t.Fatal(err)
	}
	e := &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 404 Not Found\r\nContent-Length: 5\r\n\r\nhello")}}}
	if err := Run(&o, e); err != nil {
		t.Fatal(err)
	}
	if e.file.String() != "HTTP/1.1 404 Not Found\r\nContent-Length: 5\r\n\r\nhello" || string(e.path) != "saved" {
		t.Fatalf("file %q path %q", e.file.String(), e.path)
	}
}

func TestPreservedPostAndAddressFallback(t *testing.T) {
	var o Options
	if err := Parse([][]byte{[]byte("-L"), []byte("-d"), []byte("x=1"), []byte("http://http.test/post")}, &o); err != nil {
		t.Fatal(err)
	}
	e := &mockEnv{failFirst: true, conns: []*mockConn{nil, {read: []byte("HTTP/1.1 307 Temporary Redirect\r\nLocation: /next\r\nContent-Length: 0\r\n\r\n")}, {read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")}}}
	if err := Run(&o, e); err != nil {
		t.Fatal(err)
	}
	if e.stdout.String() != "ok" || e.connected != 3 || !bytes.HasPrefix(e.conns[2].written, []byte("POST /next HTTP/1.1\r\n")) || !bytes.HasSuffix(e.conns[2].written, []byte("x=1")) {
		t.Fatalf("307 or fallback: %+v", e)
	}
}

func TestRemoteNameRejectsEmptyPath(t *testing.T) {
	var o Options
	if err := Parse([][]byte{[]byte("-O"), []byte("http://192.0.2.53/")}, &o); err != nil {
		t.Fatal(err)
	}
	e := &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")}}}
	if err := Run(&o, e); err != ErrURL || !e.conns[0].closed {
		t.Fatalf("empty filename: %v", err)
	}
}
