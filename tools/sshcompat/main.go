// sshcompat adds a synchronous server pump to pinned x/crypto/ssh sources.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"goos/tools/internal/compatroot"
	"os"
	"path/filepath"
	"strings"
)

const version = "v0.53.0"

// SHA256 of canonical JSON mapping every upstream module file to its SHA256.
const moduleTreePin = "35b0a78f570de091d5405476ca156115f0e580a94d9ffab8a13e013f51e42e3f"

var pins = map[string]string{
	"ssh/handshake.go": "c78205b40ce49adb361c2777403ee8a15de307e745bfa107bc8930677ba6821e",
	"ssh/transport.go": "cccb1f1e8120dcf878778d5a4f305cb1deb346e67307aaf509c957ca0cfb8f43",
	"ssh/cipher.go":    "dcb41888bb63e09f5f2e8d8166b12d43e76cf4f70484b89cf0f9e67d6d3d63dd",
	"ssh/server.go":    "d82a905ab4ee5b495226390df5e7ba33b0569bb3c811b002787ad55477bf42fa",
	"go.mod":           "e6419f476512a8f5d90ccb9fc71e22df4849eff2316e0f938dc82cf4f11bbe5f",
}

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sshcompat:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 4 && args[0] == "--native-key-cache" {
		return nativeKeyCache(args[1], args[2], args[3])
	}
	if len(args) != 3 {
		return errors.New("usage: sshcompat SOURCE_XCRYPTO EMPTY_OUTPUT_ROOT ADAPTER_DIRECTORY")
	}
	output, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(output)
	if err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || real != output || !compatroot.Allowed(real) || len(entries) != 0 {
		return errors.New("output must be a real empty directory under the GoOS build directory")
	}
	sources := map[string]string{}
	err = filepath.WalkDir(args[0], func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(args[0], path)
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("source contains a symlink")
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return errors.New("source contains a non-regular file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources[rel] = hash(b)
		return nil
	})
	if err != nil {
		return err
	}
	for name, pin := range pins {
		if sources[name] != pin {
			return fmt.Errorf("source SHA256 mismatch: %s", name)
		}
	}
	tree, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	if hash(tree) != moduleTreePin {
		return errors.New("complete x/crypto module source tree SHA256 mismatch")
	}
	requested := output
	stage, err := os.MkdirTemp(filepath.Dir(output), ".sshcompat-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	output = stage
	// Stage all patches; any validation failure leaves the requested root empty.
	for name := range sources {
		b, err := os.ReadFile(filepath.Join(args[0], name))
		if err != nil {
			return err
		}
		if hash(b) != sources[name] {
			return fmt.Errorf("source changed while generating: %s", name)
		}
		switch name {
		case "ssh/transport.go":
			b, err = patchTransport(b)
		case "ssh/cipher.go":
			b, err = patchCipher(b)
		case "ssh/server.go":
			b, err = patchAuth(b)
		case "ssh/handshake.go":
			b, err = replace(b, "io.ReadFull(t.config.Rand, msg.Cookie[:])", "if _, err := io.ReadFull(t.config.Rand, msg.Cookie[:]); err != nil { return err }")
		}
		if err != nil {
			return err
		}
		dst := filepath.Join(output, name)
		if err = os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err = os.WriteFile(dst, b, 0644); err != nil {
			return err
		}
	}
	h, err := os.ReadFile(filepath.Join(args[0], "ssh/handshake.go"))
	if err != nil {
		return err
	}
	if hash(h) != sources["ssh/handshake.go"] {
		return errors.New("handshake source changed while generating")
	}
	split, err := splitKEX(string(h))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(output, "ssh/goos_kex.go"), []byte(split), 0644); err != nil {
		return err
	}
	adapters := map[string]string{}
	for _, name := range []string{"pump.go.txt", "pump_test.go.txt"} {
		b, err := os.ReadFile(filepath.Join(args[2], name))
		if err != nil {
			return err
		}
		adapters[name] = hash(b)
		b, err = format.Source(b)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err = os.WriteFile(filepath.Join(output, "ssh/goos_"+strings.TrimSuffix(name, ".txt")), b, 0644); err != nil {
			return err
		}
	}
	outputs := map[string]string{}
	for _, name := range []string{"handshake.go", "transport.go", "cipher.go", "server.go", "goos_kex.go", "goos_pump.go", "goos_pump_test.go"} {
		b, err := os.ReadFile(filepath.Join(output, "ssh", name))
		if err != nil {
			return err
		}
		outputs[name] = hash(b)
	}
	record := struct {
		Version                    string `json:"version"`
		Sources, Adapters, Outputs map[string]string
	}{version, sources, adapters, outputs}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(output, "sshcompat.json"), append(b, '\n'), 0644); err != nil {
		return err
	}
	entries, err = os.ReadDir(requested)
	if err != nil || len(entries) != 0 {
		return errors.New("output changed while generating compatibility module")
	}
	if err = os.Chmod(stage, info.Mode().Perm()); err != nil {
		return err
	}
	if err = os.Remove(requested); err != nil {
		return err
	}
	if err = os.Rename(stage, requested); err != nil {
		return errors.Join(err, os.Mkdir(requested, info.Mode().Perm()))
	}
	return nil
}
func replace(b []byte, old, new string) ([]byte, error) {
	if bytes.Count(b, []byte(old)) != 1 {
		return nil, fmt.Errorf("anchor does not match once: %.80s", old)
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1), nil
}
func patchTransport(b []byte) ([]byte, error) {
	var err error
	b, err = replace(b, "initialKEXDone bool", "initialKEXDone bool\n goosBorrow bool")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "fresh := make([]byte, len(packet))", "if s.goosBudget {\n s.goosPackets++; s.goosBytes += uint32(len(packet))\n if s.goosPackets>64 || s.goosBytes>128*1024 { return nil, errors.New(\"ssh: preauthentication packet budget exceeded\") }\n }\n if s.goosBorrow { return packet, err }\n fresh := make([]byte, len(packet))")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "pendingKeyChange chan packetCipher", "pendingKeyChange chan packetCipher\n goosBorrow, goosBudget bool\n goosPackets, goosBytes uint32")
	if err != nil {
		return nil, err
	}
	// Poll must consume exactly one staged frame, including ignored packets.
	b, err = replace(b, "for {\n\t\tp, err = t.reader.readPacket", "for {\n\t\tp, err = t.reader.readPacket")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "if len(p) == 0 || (t.strictMode", "if t.goosBorrow || len(p) == 0 || (t.strictMode")
	if err != nil {
		return nil, err
	}
	return format.Source(b)
}
func patchCipher(b []byte) ([]byte, error) {
	var err error
	b, err = replace(b, "maxPacket = 256 * 1024", "maxPacket = 32 * 1024")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "aead: aead,\n\t\tiv:   iv,", "aead: aead,\n\t\tiv: iv,\n buf: make([]byte, 0, maxPacket+gcmTagSize),")
	if err != nil {
		return nil, err
	}
	// Upstream write GCM does not enforce maxPacket. The pump bounds payloads.
	return format.Source(b)
}
func patchAuth(b []byte) ([]byte, error) {
	var err error
	b, err = replace(b, "if authAttempts >= maxAuthServerAttempts {", "if authAttempts >= 16 {")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "if userAuthReq.Service != serviceSSH {", "if len(userAuthReq.User)==0 || len(userAuthReq.User)>64 { return nil, errors.New(\"ssh: username exceeds native bound\") }\n if userAuthReq.Service != serviceSSH {")
	if err != nil {
		return nil, err
	}
	b, err = replace(b, "pubKey, err := ParsePublicKey(pubKeyData)", "if algo != KeyAlgoED25519 || len(pubKeyData) != 51 { return nil, errors.New(\"ssh: only raw Ed25519 user keys accepted\") }\n pubKey, err := ParsePublicKey(pubKeyData)")
	if err != nil {
		return nil, err
	}
	return format.Source(b)
}

// Split the pinned upstream exchange at its blocking read boundary. Negotiation,
// ECDH, host signing, key derivation and NEWKEYS remain upstream code.
func splitKEX(source string) (string, error) {
	start := strings.Index(source, "func (t *handshakeTransport) enterKeyExchange(otherInitPacket []byte) error {")
	if start < 0 {
		return "", errors.New("KEX function anchor missing")
	}
	end := strings.Index(source[start:], "\n// algorithmSignerWrapper") + start
	if end < start {
		return "", errors.New("KEX function anchors missing")
	}
	body := source[start:end]
	a := strings.Index(body, "\tkex, ok := kexAlgoMap")
	c := strings.Index(body, "\tif packet, err := t.conn.readPacket();")
	if a < 0 || c < a {
		return "", errors.New("KEX split anchors missing")
	}
	pre := body[:a]
	// Native server supports only curve25519 and does not read a guessed packet
	// while negotiating. The pump drops it in a separate bounded step.
	guessStart := strings.Index(pre, "\t// We don't send FirstKexFollows")
	if guessStart < 0 {
		return "", errors.New("KEX guess anchor missing")
	}
	pre = pre[:guessStart]
	pre = strings.Replace(pre, "enterKeyExchange(otherInitPacket []byte) error", "goosNegotiate(otherInitPacket []byte) (*handshakeMagics, bool, error)", 1)
	pre = strings.ReplaceAll(pre, "return err", "return nil, false, err")
	pre += "\treturn &magics, otherInit.FirstKexFollows && (clientInit.KexAlgos[0] != serverInit.KexAlgos[0] || clientInit.ServerHostKeyAlgos[0] != serverInit.ServerHostKeyAlgos[0]), nil\n}\n"
	mid := body[a:c]
	mid = strings.Replace(mid, "t.server(kex, &magics)", "t.server(kex, magics)", 1)
	mid = strings.Replace(mid, "t.client(kex, &magics)", "t.client(kex, magics)", 1)
	// Only the server branch is exposed by the pump.
	mid = "func (t *handshakeTransport) goosExchange(magics *handshakeMagics, clientInit *kexInitMsg) error {\n var err error\n isClient := false\n" + mid + "\treturn nil\n}\n"
	out := "package ssh\nimport (\"fmt\";\"log\";\"slices\";\"strings\")\n" + pre + mid
	formatted, err := format.Source([]byte(out))
	return string(formatted), err
}
