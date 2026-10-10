//go:build cryptoprobe_tls && !goos_userspace

package cryptoprobe

import (
	"io"
	"strings"
	"testing"
)

func TestTLSRejectsNoopShim(t *testing.T) {
	err := classifyTLSHandshake(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no-op shim") {
		t.Fatalf("no wire success was not classified as a shim: %v", err)
	}
}

func TestTLSRequiresPeerEvidence(t *testing.T) {
	err := Run()
	if err == nil || !strings.Contains(err.Error(), "ClientHello emitted; peer handshake and trust unverified") {
		t.Fatalf("unexpected host TLS classification: %v", err)
	}
}

func TestTLSRejectsInvalidWire(t *testing.T) {
	for _, wire := range [][]byte{{22}, {0, 3, 3, 0, 4, 1, 0, 0, 0}, {22, 3, 3, 1, 0, 1, 0, 0, 0}} {
		if err := classifyTLSHandshake(wire, io.EOF); err == nil {
			t.Fatal("invalid wire accepted")
		}
	}
}
