package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goos/tools/tlsfixture"
)

func fixtureDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, generation := range []string{"update", "rollback", "badsig"} {
		if err := os.Mkdir(filepath.Join(dir, generation), 0755); err != nil {
			t.Fatal(err)
		}
		for _, extension := range []string{"pem", "json", "sig"} {
			if err := os.WriteFile(filepath.Join(dir, generation, "ca-certificates."+extension), []byte(generation+"-"+extension), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cert, err := tlsfixture.Certificate("valid")
	if err != nil {
		t.Fatal(err)
	}
	var chain []byte
	for _, der := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newroot-cert.pem"), chain, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newroot-key-testonly.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHTTPRoutesAndPrivateKeyIsolation(t *testing.T) {
	dir := fixtureDirectory(t)
	for _, mode := range []string{"bootstrap", "newroot"} {
		srv, err := server(dir, mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/hello", "/update/ca-certificates.pem", "/rollback/ca-certificates.json", "/badsig/ca-certificates.sig", "/newroot-key-testonly.pem", "/signing-seed-testonly.raw", "/../public.key", "/update/../../newroot-key-testonly.pem"} {
			recorder := httptest.NewRecorder()
			srv.Handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
			wantStatus := http.StatusNotFound
			var want []byte
			if path == "/hello" {
				wantStatus, want = 200, []byte("hello")
				if mode == "newroot" {
					want = []byte("new-root-ok")
				}
			} else if mode == "bootstrap" && (path == "/update/ca-certificates.pem" || path == "/rollback/ca-certificates.json" || path == "/badsig/ca-certificates.sig") {
				wantStatus = 200
				want, err = os.ReadFile(filepath.Join(dir, path[1:]))
				if err != nil {
					t.Fatal(err)
				}
			}
			if recorder.Code != wantStatus || wantStatus == 200 && !bytes.Equal(recorder.Body.Bytes(), want) {
				t.Fatalf("mode=%s path=%s code=%d body=%q", mode, path, recorder.Code, recorder.Body.Bytes())
			}
		}
		recorder := httptest.NewRecorder()
		srv.Handler.ServeHTTP(recorder, httptest.NewRequest("HEAD", "/hello", nil))
		if recorder.Code != 200 || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") == "" {
			t.Fatal("HEAD response invalid")
		}
		recorder = httptest.NewRecorder()
		srv.Handler.ServeHTTP(recorder, httptest.NewRequest("POST", "/hello", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatal("POST accepted")
		}
	}
}

func TestStandardTLS13HTTP11(t *testing.T) {
	dir := fixtureDirectory(t)
	for _, mode := range []string{"bootstrap", "newroot"} {
		srv, err := server(dir, mode)
		if err != nil {
			t.Fatal(err)
		}
		peer := httptest.NewUnstartedServer(srv.Handler)
		peer.TLS = srv.TLSConfig.Clone()
		peer.StartTLS()
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(tlsfixture.RootPEM())
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "tls.test", MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, Time: func() time.Time { return time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC) }}}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get(peer.URL + "/hello")
		if err != nil {
			peer.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		transport.CloseIdleConnections()
		peer.Close()
		want := "hello"
		if mode == "newroot" {
			want = "new-root-ok"
		}
		if err != nil || string(body) != want || response.Proto != "HTTP/1.1" || response.TLS.Version != tls.VersionTLS13 {
			t.Fatalf("TLS response = %s %q, err=%v", response.Proto, body, err)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := server(t.TempDir(), "unknown"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := server(t.TempDir(), "bootstrap"); err == nil {
		t.Fatal("missing package files accepted")
	}
	if _, err := server(t.TempDir(), "newroot"); err == nil {
		t.Fatal("missing keypair accepted")
	}
	if err := run([]string{"-fixtures", t.TempDir(), "-listen", "0.0.0.0:0"}); err == nil {
		t.Fatal("public listener accepted")
	}
}
