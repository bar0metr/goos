package fetch

import (
	"bytes"
	"errors"
	"testing"

	"goos/userspace/network"
)

type tlsCall struct {
	address network.IPv4
	port    uint16
	host    string
}

type secureEnv struct {
	*mockEnv
	tlsCalls      []tlsCall
	plainConnects int
	tlsErr        error
}

func (e *secureEnv) Connect(address network.IPv4, port uint16) (Connection, error) {
	e.plainConnects++
	return e.mockEnv.Connect(address, port)
}

func (e *secureEnv) ConnectTLS(address network.IPv4, port uint16, host []byte) (Connection, error) {
	e.tlsCalls = append(e.tlsCalls, tlsCall{address: address, port: port, host: string(host)})
	if e.tlsErr != nil {
		return nil, e.tlsErr
	}
	conn := e.conns[e.connected]
	e.connected++
	return conn, nil
}

func TestParseHTTPSURL(t *testing.T) {
	var url URL
	if err := ParseURL([]byte("https://Example.test/?q=1"), &url); err != nil {
		t.Fatal(err)
	}
	if !url.Secure || url.Port != 443 || string(url.HostBytes()) != "example.test" || string(url.TargetBytes()) != "/?q=1" {
		t.Fatalf("HTTPS URL = %+v", url)
	}
	if err := ParseURL([]byte("https://tls.test:8443/"), &url); err != nil || !url.Secure || url.Port != 8443 {
		t.Fatalf("explicit HTTPS port: %+v err=%v", url, err)
	}
}

func TestHTTPSUsesAuthenticatedTransportForNumericIP(t *testing.T) {
	var options Options
	if err := Parse([][]byte{[]byte("-I"), []byte("https://10.0.2.2/hello")}, &options); err != nil {
		t.Fatal(err)
	}
	env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")}}}}
	if err := Run(&options, env); err != nil {
		t.Fatal(err)
	}
	if env.plainConnects != 0 || env.resolved != 0 || len(env.tlsCalls) != 1 || env.tlsCalls[0].host != "10.0.2.2" || env.tlsCalls[0].port != 443 {
		t.Fatalf("transport calls: plain=%d resolve=%d TLS=%+v", env.plainConnects, env.resolved, env.tlsCalls)
	}
	if env.stdout.String() != "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n" {
		t.Fatalf("-I output = %q", env.stdout.String())
	}
}

func TestHTTPSFollowRelativeRedirectStaysSecure(t *testing.T) {
	var options Options
	if err := Parse([][]byte{[]byte("-L"), []byte("-o"), []byte("saved"), []byte("https://tls.test/start")}, &options); err != nil {
		t.Fatal(err)
	}
	env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{
		{read: []byte("HTTP/1.1 302 Found\r\nLocation: /hello\r\nContent-Length: 0\r\n\r\n")},
		{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")},
	}}}
	if err := Run(&options, env); err != nil {
		t.Fatal(err)
	}
	if env.plainConnects != 0 || len(env.tlsCalls) != 2 || env.tlsCalls[0].host != "tls.test" || env.tlsCalls[1].host != "tls.test" {
		t.Fatalf("redirect transport calls: plain=%d TLS=%+v", env.plainConnects, env.tlsCalls)
	}
	if !bytes.HasPrefix(env.conns[1].written, []byte("GET /hello HTTP/1.1\r\n")) || env.file.String() != "hello" || string(env.path) != "saved" {
		t.Fatalf("redirect result: request=%q file=%q path=%q", env.conns[1].written, env.file.String(), env.path)
	}
}

func TestRedirectSecurityTransitions(t *testing.T) {
	t.Run("HTTP to HTTPS is allowed", func(t *testing.T) {
		var options Options
		if err := Parse([][]byte{[]byte("-L"), []byte("http://http.test/start")}, &options); err != nil {
			t.Fatal(err)
		}
		env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{
			{read: []byte("HTTP/1.1 302 Found\r\nLocation: https://tls.test/hello\r\nContent-Length: 0\r\n\r\n")},
			{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")},
		}}}
		if err := Run(&options, env); err != nil {
			t.Fatal(err)
		}
		if env.plainConnects != 1 || len(env.tlsCalls) != 1 || env.stdout.String() != "ok" {
			t.Fatalf("calls plain=%d TLS=%+v output=%q", env.plainConnects, env.tlsCalls, env.stdout.String())
		}
	})
	t.Run("HTTPS to HTTPS is allowed", func(t *testing.T) {
		var options Options
		if err := Parse([][]byte{[]byte("-L"), []byte("https://tls.test/start")}, &options); err != nil {
			t.Fatal(err)
		}
		env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{
			{read: []byte("HTTP/1.1 302 Found\r\nLocation: https://tls.test/hello\r\nContent-Length: 0\r\n\r\n")},
			{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")},
		}}}
		if err := Run(&options, env); err != nil {
			t.Fatal(err)
		}
		if env.plainConnects != 0 || len(env.tlsCalls) != 2 || env.stdout.String() != "ok" {
			t.Fatalf("calls plain=%d TLS=%+v output=%q", env.plainConnects, env.tlsCalls, env.stdout.String())
		}
	})
	t.Run("HTTPS downgrade is refused before next connect", func(t *testing.T) {
		var options Options
		if err := Parse([][]byte{[]byte("-L"), []byte("https://tls.test/start")}, &options); err != nil {
			t.Fatal(err)
		}
		env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{{read: []byte("HTTP/1.1 302 Found\r\nLocation: http://http.test/hello\r\nContent-Length: 0\r\n\r\n")}}}}
		if err := Run(&options, env); err != ErrDowngrade {
			t.Fatalf("Run error = %v", err)
		}
		if env.plainConnects != 0 || len(env.tlsCalls) != 1 || env.stdout.Len() != 0 || env.path != nil || !env.conns[0].closed {
			t.Fatalf("downgrade side effects: %+v", env)
		}
	})
}

func TestHTTPSUnavailableOrFailedBeforeRequest(t *testing.T) {
	var options Options
	if err := Parse([][]byte{[]byte("-o"), []byte("saved"), []byte("https://tls.test/hello")}, &options); err != nil {
		t.Fatal(err)
	}
	t.Run("no secure network", func(t *testing.T) {
		env := &mockEnv{}
		if err := Run(&options, env); err != ErrTLSUnavailable {
			t.Fatalf("Run error = %v", err)
		}
		if env.connected != 0 || env.stdout.Len() != 0 || env.path != nil {
			t.Fatalf("unexpected HTTPS side effects: %+v", env)
		}
	})
	t.Run("TLS failure", func(t *testing.T) {
		env := &secureEnv{mockEnv: &mockEnv{}, tlsErr: errors.New("authentication failed")}
		if err := Run(&options, env); err == nil {
			t.Fatal("TLS failure succeeded")
		}
		if len(env.tlsCalls) != 1 || env.plainConnects != 0 || env.connected != 0 || env.stdout.Len() != 0 || env.path != nil {
			t.Fatalf("unexpected TLS failure side effects: %+v", env)
		}
	})
}
