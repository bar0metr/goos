package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestReplacementFailsClosed(t *testing.T) {
	for _, b := range [][]byte{[]byte("none"), []byte("a a")} {
		if _, err := replace(b, "a", "b"); err == nil {
			t.Fatal("ambiguous anchor accepted")
		}
	}
}
func TestSplitKEXMissingAnchors(t *testing.T) {
	for _, source := range []string{"", "func (t *handshakeTransport) enterKeyExchange(otherInitPacket []byte) error {"} {
		if _, err := splitKEX(source); err == nil {
			t.Fatal("missing anchor accepted")
		}
	}
}
func TestPumpAdapterHasNoGoStatements(t *testing.T) {
	b, err := os.ReadFile("pump.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "pump.go", b, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(node ast.Node) bool {
		if _, ok := node.(*ast.GoStmt); ok {
			t.Error("pump starts goroutine")
		}
		return true
	})
}
func TestPinnedGenerationReproducible(t *testing.T) {
	source := os.Getenv("SSHCOMPAT_TEST_SOURCE")
	if source == "" {
		t.Skip("set SSHCOMPAT_TEST_SOURCE to checksum-verified v0.53.0 module")
	}
	base, err := os.MkdirTemp("/Volumes/Data/tmp", "goos-sshcompat-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	outputs := []string{filepath.Join(base, "one"), filepath.Join(base, "two")}
	for _, output := range outputs {
		if err = os.Mkdir(output, 0700); err != nil {
			t.Fatal(err)
		}
		if err = run([]string{source, output, "."}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := os.ReadFile(filepath.Join(outputs[0], "sshcompat.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(outputs[1], "sshcompat.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("manifest differs across generation")
	}
	var m struct {
		Version          string
		Sources, Outputs map[string]string
	}
	if err = json.Unmarshal(a, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != version || len(m.Sources) < 100 || len(m.Outputs) != 7 {
		t.Fatalf("incomplete manifest: %s %d %d", m.Version, len(m.Sources), len(m.Outputs))
	}
	if err = run([]string{source, outputs[0], "."}); err == nil {
		t.Fatal("nonempty output accepted")
	}
}
func TestMismatchLeavesOutputEmpty(t *testing.T) {
	base, err := os.MkdirTemp("/Volumes/Data/tmp", "goos-sshcompat-reject-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	source := filepath.Join(base, "source")
	output := filepath.Join(base, "output")
	os.Mkdir(source, 0700)
	os.Mkdir(output, 0700)
	if err = run([]string{source, output, "."}); err == nil {
		t.Fatal("unverified source accepted")
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed generation changed output")
	}
}
