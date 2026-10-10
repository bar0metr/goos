// Package truststore verifies signed certificate bundles and update sequences.
package truststore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const (
	MaxMetadata = 16 << 10
	MaxBundle   = 512 << 10
	MaxRoots    = 256
)

// Metadata is serialized in this field order, without indentation, with one LF.
type Metadata struct {
	Format       int    `json:"format"`
	Sequence     uint64 `json:"sequence"`
	Source       string `json:"source"`
	SourceURL    string `json:"source_url,omitempty"`
	SourceDate   string `json:"source_date"`
	SourceSHA256 string `json:"source_sha256"`
	BundleSHA256 string `json:"bundle_sha256"`
	Certificates int    `json:"certificates"`
	BundleBytes  int    `json:"bundle_bytes"`
}

// Package owns the exact authenticated files. Callers must not mutate them.
type Package struct {
	Metadata  Metadata
	Bundle    []byte
	Meta      []byte
	Signature []byte
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func CanonicalMetadata(meta Metadata) ([]byte, error) {
	date, err := time.Parse(time.RFC3339, meta.SourceDate)
	if meta.Format != 1 || meta.Sequence == 0 || meta.Source != "mozilla" || err != nil || date.UTC().Format(time.RFC3339) != meta.SourceDate || !validHash(meta.SourceSHA256) || !validHash(meta.BundleSHA256) || meta.Certificates < 1 || meta.Certificates > MaxRoots || meta.BundleBytes < 1 || meta.BundleBytes > MaxBundle {
		return nil, errors.New("invalid trust store metadata")
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if len(data)+1 > MaxMetadata {
		return nil, errors.New("trust store metadata exceeds size limit")
	}
	return append(data, '\n'), nil
}

// ParseMetadata rejects missing, duplicate and unknown fields through exact
// canonical re-encoding, as well as alternative number or whitespace spellings.
func ParseMetadata(data []byte) (Metadata, error) {
	var meta Metadata
	if len(data) == 0 || len(data) > MaxMetadata {
		return Metadata{}, errors.New("trust store metadata size outside bounds")
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return Metadata{}, err
	}
	canonical, err := CanonicalMetadata(meta)
	if err != nil {
		return Metadata{}, err
	}
	if !bytes.Equal(data, canonical) {
		return Metadata{}, errors.New("trust store metadata is not canonical")
	}
	return meta, nil
}

// SigningMessage binds the exact canonical metadata to the bundle digest.
func SigningMessage(meta, bundle []byte) ([]byte, error) {
	metadata, err := ParseMetadata(meta)
	if err != nil {
		return nil, err
	}
	if len(bundle) == 0 || len(bundle) > MaxBundle || len(bundle) != metadata.BundleBytes {
		return nil, errors.New("trust store bundle size mismatch")
	}
	digest := sha256.Sum256(bundle)
	if hex.EncodeToString(digest[:]) != metadata.BundleSHA256 {
		return nil, errors.New("trust store bundle digest mismatch")
	}
	message := make([]byte, 0, len("GOOS-TRUST-V1\n")+len(meta)+len(digest))
	message = append(message, "GOOS-TRUST-V1\n"...)
	message = append(message, meta...)
	return append(message, digest[:]...), nil
}

// Verify authenticates bounded input before invoking certificate validation.
// validate must reject invalid certificates and return their exact count.
func Verify(bundle, metadata, signature, publicKey []byte, validate func([]byte) (int, error)) (*Package, error) {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || validate == nil {
		return nil, errors.New("trust store verifier configuration invalid")
	}
	message, err := SigningMessage(metadata, bundle)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return nil, errors.New("trust store signature invalid")
	}
	meta, err := ParseMetadata(metadata)
	if err != nil {
		return nil, err
	}
	count, err := validate(bundle)
	if err != nil {
		return nil, err
	}
	if count != meta.Certificates {
		return nil, errors.New("trust store certificate count mismatch")
	}
	return &Package{Metadata: meta, Bundle: bytes.Clone(bundle), Meta: bytes.Clone(metadata), Signature: bytes.Clone(signature)}, nil
}

// CheckUpgrade compares verified, unmodified packages. Equal sequences permit
// only an idempotent reinstall of the exact three authenticated files.
func CheckUpgrade(current, incoming *Package) error {
	if current == nil || incoming == nil || current.Metadata.Sequence == 0 || incoming.Metadata.Sequence == 0 {
		return errors.New("trust store upgrade requires verified packages")
	}
	if incoming.Metadata.Sequence < current.Metadata.Sequence {
		return errors.New("trust store sequence rollback")
	}
	if incoming.Metadata.Sequence == current.Metadata.Sequence && (!bytes.Equal(current.Bundle, incoming.Bundle) || !bytes.Equal(current.Meta, incoming.Meta) || !bytes.Equal(current.Signature, incoming.Signature)) {
		return errors.New("trust store sequence reused for different content")
	}
	return nil
}
