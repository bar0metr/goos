package tlsclient

import (
	"bytes"
	"encoding/pem"
	"testing"

	"goos/tools/tlsfixture"
)

func TestLoadRootsAcceptsFixtureRoot(t *testing.T) {
	roots, err := LoadRoots(tlsfixture.RootPEM())
	if err != nil {
		t.Fatal(err)
	}
	if roots.Count() != 1 || roots.DERBytes() == 0 {
		t.Fatalf("roots = count %d, DER bytes %d", roots.Count(), roots.DERBytes())
	}
}

func TestLoadRootsRejectsNonCanonicalInput(t *testing.T) {
	valid := tlsfixture.RootPEM()
	truncated := valid[:len(valid)-len("-----END CERTIFICATE-----\n")]
	withHeader := bytes.Replace(valid, []byte("-----BEGIN CERTIFICATE-----\n"), []byte("-----BEGIN CERTIFICATE-----\nX-Test: fixture\n"), 1)
	largeDER := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: make([]byte, MaxDER+1)})
	tooMany := bytes.Repeat(valid, MaxRoots+1)
	for name, input := range map[string][]byte{
		"empty":               nil,
		"comments only":       []byte("# test fixture\n\n# no certificate\n"),
		"non certificate":     []byte("-----BEGIN PUBLIC KEY-----\nAQ==\n-----END PUBLIC KEY-----\n"),
		"truncated":           truncated,
		"garbage before cert": append([]byte("garbage\n"), valid...),
		"garbage after cert":  append(append([]byte{}, valid...), []byte("garbage\n")...),
		"PEM header":          withHeader,
		"oversized bundle":    make([]byte, MaxBundle+1),
		"oversized DER":       largeDER,
		"too many roots":      tooMany,
		"bad then valid":      append([]byte("not PEM\n"), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			if roots, err := LoadRoots(input); err == nil || roots != nil {
				t.Fatalf("LoadRoots accepted malformed input: roots=%v err=%v", roots, err)
			}
		})
	}
}
