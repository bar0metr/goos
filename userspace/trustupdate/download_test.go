package trustupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"goos/userspace/fetch"
	"goos/userspace/network"
	"goos/userspace/truststore"
	"io"
	"strings"
	"testing"
)

type testConn struct {
	read    []byte
	written []byte
	closed  bool
}

func (c *testConn) Read(p []byte) (int, error) {
	if len(c.read) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.read)
	c.read = c.read[n:]
	return n, nil
}
func (c *testConn) Write(p []byte) (int, error) {
	c.written = append(c.written, p...)
	return len(p), nil
}
func (c *testConn) Close() { c.closed = true }

type testEnv struct {
	conns  []*testConn
	calls  int
	failAt int
}

func (*testEnv) Resolve(_ []byte, a *[8]network.IPv4) (int, error) {
	a[0] = network.IPv4{192, 0, 2, 53}
	return 1, nil
}
func (*testEnv) Connect(network.IPv4, uint16) (fetch.Connection, error) {
	return nil, errors.New("plaintext forbidden")
}
func (e *testEnv) ConnectTLS(network.IPv4, uint16, []byte) (fetch.Connection, error) {
	e.calls++
	if e.calls == e.failAt {
		return nil, errors.New("authentication failed")
	}
	return e.conns[e.calls-1], nil
}
func (*testEnv) Open([]byte) (fetch.Output, error) {
	return nil, errors.New("filesystem output forbidden")
}
func (*testEnv) Stdout() fetch.Output { return &boundedOutput{limit: 0} }
func (*testEnv) Diagnostic([]byte)    {}
func response(status int, body []byte, chunked bool) []byte {
	if chunked {
		return []byte(fmt.Sprintf("HTTP/1.1 %d Test\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", status, len(body), body))
	}
	return append([]byte(fmt.Sprintf("HTTP/1.1 %d Test\r\nContent-Length: %d\r\n\r\n", status, len(body))), body...)
}
func fixtures(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	bundle := []byte("test roots")
	digest := sha256.Sum256(bundle)
	hash := hex.EncodeToString(digest[:])
	meta, err := truststore.CanonicalMetadata(truststore.Metadata{Format: 1, Sequence: 123, Source: "mozilla", SourceDate: "2026-10-09T00:00:00Z", SourceSHA256: hash, BundleSHA256: hash, BundleBytes: len(bundle), Certificates: 1})
	if err != nil {
		t.Fatal(err)
	}
	return bundle, meta, bytes.Repeat([]byte{1}, 64)
}
func environment(t *testing.T) *testEnv {
	bundle, meta, sig := fixtures(t)
	return &testEnv{conns: []*testConn{{read: response(200, meta, false)}, {read: response(200, sig, false)}, {read: response(200, bundle, false)}}}
}

func TestDownloadUsesOnlyHTTPSAndExactPackagePaths(t *testing.T) {
	env := environment(t)
	bundle, meta, sig, err := Download(env, "https://tls.test/update/")
	wantBundle, wantMeta, wantSig := fixtures(t)
	if err != nil || !bytes.Equal(bundle, wantBundle) || !bytes.Equal(meta, wantMeta) || !bytes.Equal(sig, wantSig) {
		t.Fatalf("download: %v", err)
	}
	for i, name := range []string{"json", "sig", "pem"} {
		if !env.conns[i].closed || !bytes.HasPrefix(env.conns[i].written, []byte("GET /update/ca-certificates."+name+" HTTP/1.1\r\n")) {
			t.Fatalf("component %d request=%q closed=%v", i, env.conns[i].written, env.conns[i].closed)
		}
	}
}
func TestRejectStatusBeforeOutput(t *testing.T) {
	for _, status := range []int{302, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			env := environment(t)
			env.conns[0].read = response(status, []byte("untrusted body"), false)
			bundle, meta, sig, err := Download(env, "https://tls.test/update")
			if err == nil || env.calls != 1 || bundle != nil || meta != nil || sig != nil || !env.conns[0].closed {
				t.Fatalf("status accepted: %d err=%v", status, err)
			}
		})
	}
}
func TestOversizeComponents(t *testing.T) {
	for index, limit := range []int{truststore.MaxMetadata, 64, truststore.MaxBundle} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%v", index, chunked), func(t *testing.T) {
				env := environment(t)
				env.conns[index].read = response(200, bytes.Repeat([]byte{'x'}, limit+1), chunked)
				if _, _, _, err := Download(env, "https://tls.test/update"); err == nil {
					t.Fatal("oversized component accepted")
				}
				if env.calls != index+1 || !env.conns[index].closed {
					t.Fatal("continued after bound failure")
				}
			})
		}
	}
}
func TestTLSFailureStopsEachComponent(t *testing.T) {
	for index := 1; index <= 3; index++ {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			env := environment(t)
			env.failAt = index
			if _, _, _, err := Download(env, "https://tls.test/update"); err == nil || env.calls != index {
				t.Fatalf("failure did not stop: err=%v calls=%d", err, env.calls)
			}
		})
	}
}
func TestTruncatedComponentFails(t *testing.T) {
	for index := 0; index < 3; index++ {
		env := environment(t)
		env.conns[index].read = []byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
		if _, _, _, err := Download(env, "https://tls.test/update"); err == nil {
			t.Fatalf("truncated component %d accepted", index)
		}
	}
}
func TestUpdateURLBoundary(t *testing.T) {
	prefix := "https://tls.test/"
	maximum := fetch.MaxURL - len("/ca-certificates.json")
	good := prefix + strings.Repeat("a", maximum-len(prefix))
	if err := ValidateURL(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{good + "a", "http://tls.test/update", "https://tls.test/update?x=1", "https://tls.test/update#fragment", "https://tls.test/update\n"} {
		if ValidateURL(bad) == nil {
			t.Fatalf("accepted URL %q", bad)
		}
	}
}
