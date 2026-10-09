package truststore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testMetadata(bundle []byte, sequence uint64) Metadata {
	digest := sha256.Sum256(bundle)
	return Metadata{Format: 1, Sequence: sequence, Source: "mozilla", SourceURL: "https://curl.se/ca/cacert.pem", SourceDate: "2026-10-09T00:00:00Z", SourceSHA256: strings.Repeat("a", 64), BundleSHA256: hex.EncodeToString(digest[:]), Certificates: 1, BundleBytes: len(bundle)}
}

// Deterministic, public test-only seeds are never production signing keys.
func testKey(value byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{value}, ed25519.SeedSize))
}

func testFiles(t *testing.T, sequence uint64) ([]byte, []byte, []byte, []byte) {
	t.Helper()
	bundle := []byte("test certificate bytes")
	meta, err := CanonicalMetadata(testMetadata(bundle, sequence))
	if err != nil {
		t.Fatal(err)
	}
	message, err := SigningMessage(meta, bundle)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey(1)
	return bundle, meta, ed25519.Sign(key, message), key.Public().(ed25519.PublicKey)
}

func TestCanonicalMetadata(t *testing.T) {
	bundle := []byte("test")
	meta := testMetadata(bundle, ^uint64(0))
	data, err := CanonicalMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"format\":1,\"sequence\":18446744073709551615,\"source\":\"mozilla\",\"source_url\":\"https://curl.se/ca/cacert.pem\",\"source_date\":\"2026-10-09T00:00:00Z\",\"source_sha256\":\"" + meta.SourceSHA256 + "\",\"bundle_sha256\":\"" + meta.BundleSHA256 + "\",\"certificates\":1,\"bundle_bytes\":4}\n"
	if string(data) != want {
		t.Fatalf("canonical = %q, want %q", data, want)
	}
	parsed, err := ParseMetadata(data)
	if err != nil || parsed != meta {
		t.Fatalf("roundtrip = %+v, %v", parsed, err)
	}
	message, err := SigningMessage(data, bundle)
	digest := sha256.Sum256(bundle)
	expected := append(append([]byte("GOOS-TRUST-V1\n"), data...), digest[:]...)
	if err != nil || !bytes.Equal(message, expected) {
		t.Fatalf("signing message = %x, %v", message, err)
	}
}

func TestRejectInvalidMetadata(t *testing.T) {
	base := testMetadata([]byte("test"), 1)
	for name, change := range map[string]func(*Metadata){
		"format":              func(m *Metadata) { m.Format = 2 },
		"sequence":            func(m *Metadata) { m.Sequence = 0 },
		"source":              func(m *Metadata) { m.Source = "other" },
		"date":                func(m *Metadata) { m.SourceDate = "not a date" },
		"offset date":         func(m *Metadata) { m.SourceDate = "2026-10-09T01:00:00+01:00" },
		"UTC offset spelling": func(m *Metadata) { m.SourceDate = "2026-10-09T00:00:00+00:00" },
		"fractional date":     func(m *Metadata) { m.SourceDate = "2026-10-09T00:00:00.1Z" },
		"upper hash":          func(m *Metadata) { m.SourceSHA256 = strings.Repeat("A", 64) },
		"short hash":          func(m *Metadata) { m.BundleSHA256 = "abcd" },
		"non hex":             func(m *Metadata) { m.BundleSHA256 = strings.Repeat("g", 64) },
		"zero count":          func(m *Metadata) { m.Certificates = 0 },
		"many roots":          func(m *Metadata) { m.Certificates = MaxRoots + 1 },
		"zero bytes":          func(m *Metadata) { m.BundleBytes = 0 },
		"large bundle":        func(m *Metadata) { m.BundleBytes = MaxBundle + 1 },
		"large metadata":      func(m *Metadata) { m.SourceURL = strings.Repeat("a", MaxMetadata) },
	} {
		t.Run(name, func(t *testing.T) {
			meta := base
			change(&meta)
			if _, err := CanonicalMetadata(meta); err == nil {
				t.Fatal("accepted invalid metadata")
			}
			data, _ := json.Marshal(meta)
			if _, err := ParseMetadata(append(data, '\n')); err == nil {
				t.Fatal("parsed invalid metadata")
			}
		})
	}
}

func TestRejectNoncanonicalMetadata(t *testing.T) {
	_, data, _, _ := testFiles(t, 1)
	for name, malformed := range map[string][]byte{
		"empty":             nil,
		"too large":         bytes.Repeat([]byte(" "), MaxMetadata+1),
		"unknown":           bytes.Replace(data, []byte("{\"format\""), []byte("{\"unknown\":1,\"format\""), 1),
		"duplicate":         bytes.Replace(data, []byte("{\"format\":1"), []byte("{\"format\":1,\"format\":1"), 1),
		"missing":           bytes.Replace(data, []byte("\"format\":1,"), nil, 1),
		"missing newline":   data[:len(data)-1],
		"whitespace":        append([]byte(" "), data...),
		"extra newline":     append(bytes.Clone(data), '\n'),
		"trailing object":   append(bytes.Clone(data), []byte("{}")...),
		"float sequence":    bytes.Replace(data, []byte("\"sequence\":1"), []byte("\"sequence\":1.0"), 1),
		"exponent sequence": bytes.Replace(data, []byte("\"sequence\":1"), []byte("\"sequence\":1e0"), 1),
		"negative sequence": bytes.Replace(data, []byte("\"sequence\":1"), []byte("\"sequence\":-1"), 1),
		"overflow sequence": bytes.Replace(data, []byte("\"sequence\":1"), []byte("\"sequence\":18446744073709551616"), 1),
		"null field":        bytes.Replace(data, []byte("\"sequence\":1"), []byte("\"sequence\":null"), 1),
		"case alias":        bytes.Replace(data, []byte("\"format\""), []byte("\"FORMAT\""), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMetadata(malformed); err == nil {
				t.Fatal("accepted noncanonical metadata")
			}
		})
	}
}

func TestVerify(t *testing.T) {
	bundle, meta, sig, key := testFiles(t, 1)
	calls := 0
	pkg, err := Verify(bundle, meta, sig, key, func(got []byte) (int, error) {
		calls++
		if !bytes.Equal(got, bundle) {
			t.Fatal("validator bundle differs")
		}
		return 1, nil
	})
	if err != nil || calls != 1 || pkg == nil || pkg.Metadata.Sequence != 1 || !bytes.Equal(pkg.Bundle, bundle) || !bytes.Equal(pkg.Meta, meta) || !bytes.Equal(pkg.Signature, sig) {
		t.Fatalf("Verify = %+v, %v; calls=%d", pkg, err, calls)
	}
	bundle[0] ^= 1
	meta[0] ^= 1
	sig[0] ^= 1
	if pkg.Bundle[0] == bundle[0] || pkg.Meta[0] == meta[0] || pkg.Signature[0] == sig[0] {
		t.Fatal("verified package aliases caller inputs")
	}
}

func TestVerifyRejectsBeforeCertificateValidation(t *testing.T) {
	for _, name := range []string{"signature", "wrong key", "short key", "long key", "short signature", "long signature", "digest", "size", "oversize", "metadata"} {
		t.Run(name, func(t *testing.T) {
			bundle, meta, sig, key := testFiles(t, 1)
			switch name {
			case "signature":
				sig[0] ^= 1
			case "wrong key":
				key = testKey(2).Public().(ed25519.PublicKey)
			case "short key":
				key = key[:31]
			case "long key":
				key = append(key, 0)
			case "short signature":
				sig = sig[:63]
			case "long signature":
				sig = append(sig, 0)
			case "digest":
				bundle[0] ^= 1
			case "size":
				bundle = append(bundle, 0)
			case "oversize":
				bundle = make([]byte, MaxBundle+1)
			case "metadata":
				meta = append(meta, '\n')
			}
			calls := 0
			pkg, err := Verify(bundle, meta, sig, key, func([]byte) (int, error) { calls++; return 1, nil })
			if err == nil || pkg != nil || calls != 0 {
				t.Fatalf("Verify = %v, %v, calls=%d", pkg, err, calls)
			}
		})
	}
}

func TestVerifyValidationFailures(t *testing.T) {
	bundle, meta, sig, key := testFiles(t, 1)
	validationErr := errors.New("invalid certificate")
	for _, check := range []struct {
		name     string
		validate func([]byte) (int, error)
	}{
		{"nil validator", nil},
		{"invalid certificate", func([]byte) (int, error) { return 0, validationErr }},
		{"count mismatch", func([]byte) (int, error) { return 2, nil }},
		{"zero count", func([]byte) (int, error) { return 0, nil }},
	} {
		t.Run(check.name, func(t *testing.T) {
			if pkg, err := Verify(bundle, meta, sig, key, check.validate); err == nil || pkg != nil {
				t.Fatalf("Verify = %v, %v", pkg, err)
			}
		})
	}
}

func testPackage(t *testing.T, sequence uint64) *Package {
	t.Helper()
	bundle, meta, sig, key := testFiles(t, sequence)
	pkg, err := Verify(bundle, meta, sig, key, func([]byte) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func TestCheckUpgrade(t *testing.T) {
	current := testPackage(t, 2)
	if err := CheckUpgrade(current, testPackage(t, 3)); err != nil {
		t.Fatal(err)
	}
	if err := CheckUpgrade(current, testPackage(t, 2)); err != nil {
		t.Fatal(err)
	}
	if err := CheckUpgrade(current, testPackage(t, 1)); err == nil {
		t.Fatal("accepted rollback")
	}
	for _, file := range []string{"bundle", "metadata", "signature"} {
		t.Run(file, func(t *testing.T) {
			incoming := testPackage(t, 2)
			switch file {
			case "bundle":
				incoming.Bundle[0] ^= 1
			case "metadata":
				incoming.Meta[0] ^= 1
			case "signature":
				incoming.Signature[0] ^= 1
			}
			if err := CheckUpgrade(current, incoming); err == nil {
				t.Fatal("accepted sequence reuse")
			}
		})
	}
	if CheckUpgrade(nil, current) == nil || CheckUpgrade(current, nil) == nil || CheckUpgrade(current, &Package{}) == nil {
		t.Fatal("accepted missing verified package")
	}
}

func TestSignedSequenceReuse(t *testing.T) {
	current := testPackage(t, 2)
	bundle, _, _, key := testFiles(t, 2)
	changed := testMetadata(bundle, 2)
	changed.SourceURL = "https://curl.se/ca/archive.pem"
	meta, err := CanonicalMetadata(changed)
	if err != nil {
		t.Fatal(err)
	}
	message, err := SigningMessage(meta, bundle)
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := Verify(bundle, meta, ed25519.Sign(testKey(1), message), key, func([]byte) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	if CheckUpgrade(current, incoming) == nil {
		t.Fatal("accepted authenticated sequence reuse")
	}
}

func TestOptionalSourceURL(t *testing.T) {
	meta := testMetadata([]byte("test"), 1)
	meta.SourceURL = ""
	data, err := CanonicalMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("source_url")) {
		t.Fatal("empty optional URL not omitted")
	}
	parsed, err := ParseMetadata(data)
	if err != nil || parsed != meta {
		t.Fatalf("optional URL roundtrip = %+v, %v", parsed, err)
	}
}
