package remotepolicy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

const (
	MaxAuthorizedKeysBytes = 64 * 1024
	MaxAuthorizedKeys      = 64
	MaxAuthorizedKeyLine   = 4096
	PublicBlobSize         = 4 + len(KeyAlgorithm) + 4 + ed25519.PublicKeySize
)

// AuthorizedKeys is a bounded allowlist of Ed25519 public keys.
type AuthorizedKeys struct {
	keys  [MaxAuthorizedKeys][ed25519.PublicKeySize]byte
	count uint8
}

func (keys *AuthorizedKeys) Len() int {
	if keys == nil {
		return 0
	}
	return int(keys.count)
}

// At returns an owned copy; callers cannot mutate the validated allowlist.
func (keys *AuthorizedKeys) At(index int) (ed25519.PublicKey, bool) {
	if keys == nil || index < 0 || index >= int(keys.count) {
		return nil, false
	}
	public := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(public, keys.keys[index][:])
	return public, true
}

// Matches always compares all slots, masking unused entries without exposing
// the allowlist size or the matching position through the comparison loop.
func (keys *AuthorizedKeys) Matches(public []byte) bool {
	if keys == nil || len(public) != ed25519.PublicKeySize {
		return false
	}
	matched := 0
	for index := 0; index < MaxAuthorizedKeys; index++ {
		active := subtle.ConstantTimeLessOrEq(index+1, int(keys.count))
		matched |= active & subtle.ConstantTimeCompare(keys.keys[index][:], public)
	}
	return matched != 0
}

// PublicBlob encodes the canonical SSH string pair for an Ed25519 public key.
func PublicBlob(public []byte) ([]byte, error) {
	if len(public) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key length")
	}
	blob := make([]byte, PublicBlobSize)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(KeyAlgorithm)))
	copy(blob[4:], KeyAlgorithm)
	offset := 4 + len(KeyAlgorithm)
	binary.BigEndian.PutUint32(blob[offset:offset+4], ed25519.PublicKeySize)
	copy(blob[offset+4:], public)
	return blob, nil
}

func ParsePublicBlob(blob []byte) (ed25519.PublicKey, error) {
	offset := 4 + len(KeyAlgorithm)
	if len(blob) != PublicBlobSize || binary.BigEndian.Uint32(blob[:4]) != uint32(len(KeyAlgorithm)) ||
		!bytes.Equal(blob[4:offset], []byte(KeyAlgorithm)) || binary.BigEndian.Uint32(blob[offset:offset+4]) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 SSH public blob")
	}
	public := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(public, blob[offset+4:])
	return public, nil
}

// Fingerprint uses the SSH public blob, not the raw Ed25519 bytes.
func Fingerprint(public []byte) (string, error) {
	blob, err := PublicBlob(public)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), nil
}

func PublicLine(public []byte) ([]byte, error) {
	blob, err := PublicBlob(public)
	if err != nil {
		return nil, err
	}
	return []byte(KeyAlgorithm + " " + base64.StdEncoding.EncodeToString(blob) + "\n"), nil
}

// ParseAuthorizedKeys rejects the entire candidate on any unsupported key,
// option, malformed blob, invalid key field, or bound violation. Comments are
// opaque, and both LF and CRLF lines are accepted. Empty input is a valid
// deny-all list; it does not enable another authentication method.
func ParseAuthorizedKeys(data []byte) (AuthorizedKeys, error) {
	var result AuthorizedKeys
	if len(data) > MaxAuthorizedKeysBytes {
		return result, errors.New("authorized keys file too large")
	}
	for len(data) != 0 {
		line := data
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			line, data = data[:end], data[end+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > MaxAuthorizedKeyLine {
			return AuthorizedKeys{}, errors.New("authorized keys line too long")
		}
		line = bytes.Trim(line, " \t")
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		if result.count == MaxAuthorizedKeys {
			return AuthorizedKeys{}, errors.New("too many authorized keys")
		}
		kind, rest := field(line)
		if string(kind) != KeyAlgorithm {
			return AuthorizedKeys{}, errors.New("unsupported authorized key or options")
		}
		encoded, _ := field(rest)
		if len(encoded) != base64.StdEncoding.EncodedLen(PublicBlobSize) {
			return AuthorizedKeys{}, errors.New("invalid authorized key encoding")
		}
		for _, c := range encoded {
			if c < 0x21 || c > 0x7e {
				return AuthorizedKeys{}, errors.New("invalid authorized key field")
			}
		}
		var blob [PublicBlobSize]byte
		n, err := base64.StdEncoding.Strict().Decode(blob[:], encoded)
		if err != nil || n != len(blob) {
			return AuthorizedKeys{}, errors.New("invalid authorized key encoding")
		}
		public, err := ParsePublicBlob(blob[:])
		if err != nil {
			return AuthorizedKeys{}, err
		}
		copy(result.keys[result.count][:], public)
		result.count++
	}
	return result, nil
}

func field(line []byte) (token, rest []byte) {
	line = bytes.TrimLeft(line, " \t")
	for index, c := range line {
		if c == ' ' || c == '\t' {
			return line[:index], bytes.TrimLeft(line[index:], " \t")
		}
	}
	return line, nil
}
