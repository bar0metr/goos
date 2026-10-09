package fetch

import (
	"strings"
	"testing"
)

func TestRedirectKeepsCredentialsAtOriginalOrigin(t *testing.T) {
	for _, target := range []string{"https://other.test/hello", "https://origin.test:9443/hello"} {
		t.Run(target, func(t *testing.T) {
			first := &mockConn{read: []byte("HTTP/1.1 302 Found\r\nLocation: " + target + "\r\nContent-Length: 0\r\n\r\n")}
			second := &mockConn{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")}
			env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{first, second}}}
			var opts Options
			args := [][]byte{[]byte("-L"), []byte("-H"), []byte("Authorization: Bearer secret"), []byte("-H"), []byte("Cookie: token=secret"), []byte("-H"), []byte("Proxy-Authorization: secret"), []byte("-H"), []byte("Host: override.test"), []byte("-H"), []byte("X-Test: kept"), []byte("https://origin.test/")}
			if err := Parse(args, &opts); err != nil {
				t.Fatal(err)
			}
			if err := Run(&opts, env); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(first.written), "Authorization: Bearer secret") {
				t.Fatal("original request lost caller credentials")
			}
			redirected := string(second.written)
			for _, forbidden := range []string{"Authorization:", "Cookie:", "secret", "override.test"} {
				if strings.Contains(redirected, forbidden) {
					t.Fatalf("cross-origin request exposed %q: %q", forbidden, redirected)
				}
			}
			if !strings.Contains(redirected, "X-Test: kept") {
				t.Fatal("redirect lost non-sensitive header")
			}
		})
	}
}

func TestSameOriginRedirectRetainsAuthorization(t *testing.T) {
	first := &mockConn{read: []byte("HTTP/1.1 302 Found\r\nLocation: /hello\r\nContent-Length: 0\r\n\r\n")}
	second := &mockConn{read: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")}
	env := &secureEnv{mockEnv: &mockEnv{conns: []*mockConn{first, second}}}
	var opts Options
	if err := Parse([][]byte{[]byte("-L"), []byte("-H"), []byte("Authorization: Bearer secret"), []byte("https://origin.test/")}, &opts); err != nil {
		t.Fatal(err)
	}
	if err := Run(&opts, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second.written), "Authorization: Bearer secret") {
		t.Fatal("same-origin request lost authorization")
	}
}
