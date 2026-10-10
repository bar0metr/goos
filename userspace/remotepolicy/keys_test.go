package remotepolicy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

func testPublic(fill byte) ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
}

func keyLine(t *testing.T, public []byte) string {
	t.Helper()
	line, err := PublicLine(public)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func TestAuthorizedKeysCommentsAndMatching(t *testing.T) {
	first, second := testPublic(1), testPublic(2)
	input := "\n\t \n # comment\n" + strings.TrimSuffix(keyLine(t, first), "\n") + " user@example\n\t" + strings.TrimSuffix(keyLine(t, second), "\n") + "\tsecond comment"
	keys, err := ParseAuthorizedKeys([]byte(input))
	if err != nil || keys.Len() != 2 {
		t.Fatalf("parse = %d, %v", keys.Len(), err)
	}
	if !keys.Matches(first) || !keys.Matches(second) || keys.Matches(testPublic(3)) || keys.Matches(first[:31]) || keys.Matches(append(first, 0)) {
		t.Fatal("incorrect public key matching")
	}
	var missing *AuthorizedKeys
	if missing.Len() != 0 || missing.Matches(first) {
		t.Fatal("nil list authorized key")
	}
	for _, input := range []string{"", " \n# comment\n"} {
		keys, err := ParseAuthorizedKeys([]byte(input))
		if err != nil || keys.Len() != 0 || keys.Matches(first) {
			t.Fatalf("empty list = %d, %v", keys.Len(), err)
		}
	}
}

func TestAuthorizedKeysOpaqueCommentsAndCRLF(t *testing.T) {
	public := testPublic(1)
	line := strings.TrimSuffix(keyLine(t, public), "\n")
	for _, input := range []string{
		"# operator café\r\n\t\r\n" + line + " user@café\r\n",
		line + " ignored\x00comment\r\n",
		"# opaque\x00comment\r\n" + line + "\r\n",
		line + " " + strings.Repeat("c", MaxAuthorizedKeyLine-len(line)-1) + "\r\n",
	} {
		keys, err := ParseAuthorizedKeys([]byte(input))
		if err != nil || keys.Len() != 1 || !keys.Matches(public) {
			t.Fatalf("opaque comment or CRLF rejected: %d keys, %v", keys.Len(), err)
		}
	}
}

func TestAuthorizedKeysRejectsEntireCandidate(t *testing.T) {
	public := testPublic(1)
	valid := keyLine(t, public)
	blob, _ := PublicBlob(public)
	malformedBlob := func(mutator func([]byte)) string {
		candidate := append([]byte(nil), blob...)
		mutator(candidate)
		return KeyAlgorithm + " " + base64.StdEncoding.EncodeToString(candidate) + "\n"
	}
	bad := []string{
		"ssh-rsa AAAA comment\n", "ssh-ed25519\n", "ssh-ed25519 !!!\n", "ssh-ed25519 AAAA\n",
		"command=\"sh\" " + valid, "no-pty " + valid,
		"environment=\"A=B\",no-pty " + valid, "cert-authority " + valid,
		strings.TrimSuffix(valid, "\n") + "\x00comment\n", "ssh-ed25519\r AAAA\n",
		"ssh-ed25519\x00 AAAA\n", strings.Repeat(" ", MaxAuthorizedKeyLine+1) + "\n",
		malformedBlob(func(b []byte) { b[4] = 'x' }),
		malformedBlob(func(b []byte) { binary.BigEndian.PutUint32(b[:4], 10) }),
		malformedBlob(func(b []byte) { binary.BigEndian.PutUint32(b[15:19], 31) }),
		KeyAlgorithm + " " + base64.StdEncoding.EncodeToString(append(blob, 0)) + "\n",
		KeyAlgorithm + " " + base64.StdEncoding.EncodeToString(blob[:len(blob)-1]) + "\n",
	}
	// The 51-byte canonical blob has no Base64 padding.
	encoded := base64.StdEncoding.EncodeToString(blob)
	bad = append(bad, KeyAlgorithm+" "+encoded+"=\n")
	for _, control := range []byte{0, '\r', 1, 0x7f} {
		bad = append(bad, KeyAlgorithm+" "+encoded[:10]+string(control)+encoded[11:]+"\n")
	}
	for index, invalid := range bad {
		keys, err := ParseAuthorizedKeys([]byte(valid + invalid))
		if err == nil || keys.Len() != 0 || keys.Matches(public) {
			t.Errorf("candidate %d exposed a partial allowlist: %d, %v", index, keys.Len(), err)
		}
	}
}

func TestAuthorizedKeysBounds(t *testing.T) {
	line := keyLine(t, testPublic(1))
	keys, err := ParseAuthorizedKeys([]byte(strings.Repeat(line, MaxAuthorizedKeys)))
	if err != nil || keys.Len() != MaxAuthorizedKeys {
		t.Fatalf("64 keys = %d, %v", keys.Len(), err)
	}
	if keys, err := ParseAuthorizedKeys([]byte(strings.Repeat(line, MaxAuthorizedKeys+1))); err == nil || keys.Len() != 0 {
		t.Fatal("65 keys accepted")
	}
	boundedLine := strings.TrimSuffix(line, "\n") + " " + strings.Repeat("c", MaxAuthorizedKeyLine-len(line))
	if len(boundedLine) != MaxAuthorizedKeyLine {
		t.Fatal("invalid test fixture")
	}
	if keys, err := ParseAuthorizedKeys([]byte(boundedLine)); err != nil || keys.Len() != 1 {
		t.Fatalf("4096-byte line = %d, %v", keys.Len(), err)
	}
	if _, err := ParseAuthorizedKeys([]byte(boundedLine + "c")); err == nil {
		t.Fatal("4097-byte line accepted")
	}
	data := bytes.Repeat([]byte("#"+strings.Repeat("c", 1022)+"\n"), 64)
	if len(data) != MaxAuthorizedKeysBytes {
		t.Fatal("invalid file-bound fixture")
	}
	if _, err := ParseAuthorizedKeys(data); err != nil {
		t.Fatalf("64 KiB file rejected: %v", err)
	}
	if _, err := ParseAuthorizedKeys(append(data, '\n')); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestPublicWireEncodingAndFingerprint(t *testing.T) {
	// Fixed public bytes keep this wire-format check independent of key generation.
	public := make([]byte, ed25519.PublicKeySize)
	for index := range public {
		public[index] = byte(index)
	}
	want := append([]byte{0, 0, 0, 11}, []byte("ssh-ed25519")...)
	want = append(want, 0, 0, 0, 32)
	want = append(want, public...)
	blob, err := PublicBlob(public)
	if err != nil || !bytes.Equal(blob, want) {
		t.Fatalf("public blob = %x, %v", blob, err)
	}
	parsed, err := ParsePublicBlob(blob)
	if err != nil || !bytes.Equal(parsed, public) {
		t.Fatalf("parse public blob = %x, %v", parsed, err)
	}
	parsed[0] = 99
	if blob[19] != 0 {
		t.Fatal("parsed key aliases input")
	}
	fingerprint, err := Fingerprint(public)
	digest := sha256.Sum256(want)
	if err != nil || fingerprint != "SHA256:"+base64.RawStdEncoding.EncodeToString(digest[:]) || strings.Contains(fingerprint, "=") {
		t.Fatalf("fingerprint = %q, %v", fingerprint, err)
	}
	for _, bad := range [][]byte{nil, public[:31], append(public, 0)} {
		if _, err := PublicBlob(bad); err == nil {
			t.Fatal("invalid public key accepted")
		}
		if _, err := PublicLine(bad); err == nil {
			t.Fatal("invalid public line accepted")
		}
		if _, err := Fingerprint(bad); err == nil {
			t.Fatal("invalid fingerprint input accepted")
		}
	}
	for _, bad := range [][]byte{nil, blob[:len(blob)-1], append(blob, 0)} {
		if _, err := ParsePublicBlob(bad); err == nil {
			t.Fatal("invalid public blob accepted")
		}
	}
}

func TestAuthorizedKeysUnusedSlotsNeverMatch(t *testing.T) {
	var empty AuthorizedKeys
	zeroPublic := make([]byte, ed25519.PublicKeySize)
	if empty.Matches(zeroPublic) {
		t.Fatal("unused empty slot authorized zero public key")
	}
	keys, err := ParseAuthorizedKeys([]byte(keyLine(t, testPublic(1))))
	if err != nil || keys.Matches(zeroPublic) {
		t.Fatal("unused slot authorized zero public key")
	}
	keys, err = ParseAuthorizedKeys([]byte(keyLine(t, zeroPublic)))
	if err != nil || !keys.Matches(zeroPublic) {
		t.Fatal("explicitly configured public key not matched")
	}
}

func TestAuthorizedKeysAtReturnsOwnedCopy(t *testing.T) {
	first, second := testPublic(1), testPublic(2)
	keys, err := ParseAuthorizedKeys([]byte(keyLine(t, first) + keyLine(t, second)))
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []ed25519.PublicKey{first, second} {
		public, ok := keys.At(index)
		if !ok || !bytes.Equal(public, want) {
			t.Fatalf("At(%d) = %x, %v", index, public, ok)
		}
		public[0] ^= 1
		if !keys.Matches(want) {
			t.Fatal("returned public key aliases allowlist")
		}
	}
	for _, index := range []int{-1, keys.Len(), MaxAuthorizedKeys, 10000} {
		if public, ok := keys.At(index); ok || public != nil {
			t.Fatalf("At(%d) accepted invalid index", index)
		}
	}
	var missing *AuthorizedKeys
	if public, ok := missing.At(0); ok || public != nil {
		t.Fatal("nil list exposes public key")
	}
}

func FuzzParseAuthorizedKeys(f *testing.F) {
	f.Add([]byte("# comment\n"))
	f.Add([]byte("ssh-ed25519 AAAA\n"))
	line, _ := PublicLine(testPublic(1))
	f.Add(line)
	f.Fuzz(func(t *testing.T, data []byte) {
		keys, err := ParseAuthorizedKeys(data)
		if keys.Len() > MaxAuthorizedKeys || (err != nil && keys.Len() != 0) {
			t.Fatal("parser violated candidate bounds")
		}
	})
}
