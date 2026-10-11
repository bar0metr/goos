package remotepolicy

import (
	"bytes"
	"encoding/base64"
	"errors"
)

const MaxKeyComment = 64

// KeyEntry contains a canonical fingerprint and a terminal-safe, bounded comment.
type KeyEntry struct{ Fingerprint, Comment string }

// ListAuthorizedKeys validates the complete file before returning entries in file order.
func ListAuthorizedKeys(data []byte) ([]KeyEntry, error) {
	keys, err := ParseAuthorizedKeys(data)
	if err != nil {
		return nil, err
	}
	entries := make([]KeyEntry, 0, keys.Len())
	for len(data) != 0 {
		line, rest := authorizedLine(data)
		data = rest
		trimmed := bytes.Trim(line, " \t\r")
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		_, fields := field(trimmed)
		_, comment := field(fields)
		public, _ := keys.At(len(entries))
		fingerprint, err := Fingerprint(public)
		if err != nil {
			return nil, err
		}
		safe := make([]byte, min(len(comment), MaxKeyComment))
		for i := range safe {
			c := comment[i]
			if c < 0x20 || c > 0x7e {
				c = '?'
			}
			safe[i] = c
		}
		entries = append(entries, KeyEntry{Fingerprint: fingerprint, Comment: string(safe)})
	}
	return entries, nil
}

// AddAuthorizedKey appends one validated Ed25519 line without rewriting existing bytes.
func AddAuthorizedKey(data, line []byte) ([]byte, error) {
	old, err := ParseAuthorizedKeys(data)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || len(line) > MaxAuthorizedKeyLine || bytes.IndexAny(line, "\r\n") >= 0 {
		return nil, errors.New("expected one Ed25519 public key line")
	}
	added, err := ParseAuthorizedKeys(line)
	if err != nil {
		return nil, err
	}
	if added.Len() != 1 {
		return nil, errors.New("expected one Ed25519 public key line")
	}
	public, _ := added.At(0)
	if old.Matches(public) {
		return nil, errors.New("public key already authorized")
	}
	extra := len(line) + 1
	if len(data) > 0 && data[len(data)-1] != '\n' {
		extra++
	}
	if len(data)+extra > MaxAuthorizedKeysBytes || old.Len() == MaxAuthorizedKeys {
		return nil, errors.New("authorized keys limit exceeded")
	}
	result := make([]byte, 0, len(data)+extra)
	result = append(result, data...)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, line...)
	result = append(result, '\n')
	if _, err = ParseAuthorizedKeys(result); err != nil {
		return nil, err
	}
	return result, nil
}

func ValidFingerprint(value []byte) bool {
	if len(value) != 50 || !bytes.HasPrefix(value, []byte("SHA256:")) {
		return false
	}
	var digest [32]byte
	n, err := base64.RawStdEncoding.Strict().Decode(digest[:], value[7:])
	return err == nil && n == len(digest)
}

// RemoveAuthorizedKey removes exactly one matching complete line. Duplicate
// fingerprints are ambiguous, even when their comments differ.
func RemoveAuthorizedKey(data, fingerprint []byte) ([]byte, error) {
	if !ValidFingerprint(fingerprint) {
		return nil, errors.New("expected full canonical SHA256 fingerprint")
	}
	entries, err := ListAuthorizedKeys(data)
	if err != nil {
		return nil, err
	}
	match := -1
	for i, entry := range entries {
		if entry.Fingerprint == string(fingerprint) {
			if match >= 0 {
				return nil, errors.New("ambiguous authorized key fingerprint")
			}
			match = i
		}
	}
	if match < 0 {
		return nil, errors.New("authorized key fingerprint not found")
	}
	offset, index := 0, 0
	for rest := data; len(rest) != 0; {
		line, next := authorizedLine(rest)
		size := len(rest) - len(next)
		trimmed := bytes.Trim(line, " \t\r")
		if len(trimmed) > 0 && trimmed[0] != '#' {
			if index == match {
				result := make([]byte, 0, len(data)-size)
				result = append(result, data[:offset]...)
				result = append(result, data[offset+size:]...)
				return result, nil
			}
			index++
		}
		offset += size
		rest = next
	}
	return nil, errors.New("authorized key entry unavailable")
}

func authorizedLine(data []byte) ([]byte, []byte) {
	if end := bytes.IndexByte(data, '\n'); end >= 0 {
		return data[:end], data[end+1:]
	}
	return data, nil
}
