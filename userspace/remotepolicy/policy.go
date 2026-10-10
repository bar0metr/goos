// Package remotepolicy validates remote access settings and persistent keys.
package remotepolicy

import (
	"errors"
	"goos/userspace/config"
	"net/netip"
	"strings"
)

const (
	Prefix          = "system.remote."
	MaxPath         = 64
	KEXAlgorithm    = "curve25519-sha256"
	KeyAlgorithm    = "ssh-ed25519"
	CipherAlgorithm = "aes128-gcm@openssh.com"
)

// Policy is a complete validated snapshot. Load rejects the whole snapshot when
// any setting is missing, malformed, or outside the compiled implementation.
type Policy struct {
	Enabled            bool
	ListenAddress      [4]byte
	Port               uint16
	AuthorizedKeys     string
	HostKey            string
	MaxSessions        uint8
	MaxAuthAttempts    uint8
	HandshakeTimeoutMS uint32
	IdleTimeoutSec     uint32
	KEX                []string
	Ciphers            []string
	HostKeyAlgorithms  []string
	UserKeyAlgorithms  []string
}

type declaration struct {
	name  string
	value config.Value
}

func text(value string) config.Value {
	result, _ := config.Text([]byte(value))
	return result
}

func defaults() [13]declaration {
	return [13]declaration{
		{"enabled", config.Bool(false)},
		{"listen-address", text("0.0.0.0")},
		{"port", config.Uint(22)},
		{"authorized-keys", text("/home/user/.ssh/authorized_keys")},
		{"host-key", text("/state/security/ssh/ssh_host_ed25519_key")},
		{"max-sessions", config.Uint(4)},
		{"max-auth-attempts", config.Uint(3)},
		{"handshake-timeout-ms", config.Uint(15000)},
		{"idle-timeout-sec", config.Uint(0)},
		{"kex", text(KEXAlgorithm)},
		{"ciphers", text(CipherAlgorithm)},
		{"host-key-algorithms", text(KeyAlgorithm)},
		{"user-key-algorithms", text(KeyAlgorithm)},
	}
}

// DeclareDefaults adds the remote namespace with writable typed defaults.
func DeclareDefaults(store *config.Store) config.Status {
	for _, item := range defaults() {
		if status := store.Declare(Prefix+item.name, item.value, false); status != config.StatusOK {
			return status
		}
	}
	return config.StatusOK
}

type Getter func(key string) (config.Value, config.Status)

func Load(get Getter) (Policy, error) {
	if get == nil {
		return Policy{}, errors.New("remote configuration unavailable")
	}
	var values [13]config.Value
	for index, item := range defaults() {
		value, status := get(Prefix + item.name)
		if status != config.StatusOK || value.Kind != item.value.Kind || !validValue(value) {
			return Policy{}, errors.New("invalid remote setting: " + item.name)
		}
		values[index] = value
	}
	addressText := string(values[1].Bytes())
	address, err := netip.ParseAddr(addressText)
	if err != nil || !address.Is4() || address.String() != addressText {
		return Policy{}, errors.New("invalid remote listen address")
	}
	if !bounded(values[2].Uint, 1, 65535) || !bounded(values[5].Uint, 1, 4) ||
		!bounded(values[6].Uint, 1, 10) || !bounded(values[7].Uint, 1000, 120000) ||
		!bounded(values[8].Uint, 0, 86400) {
		return Policy{}, errors.New("remote numeric setting outside bounds")
	}
	policy := Policy{
		Enabled: values[0].Uint == 1, ListenAddress: address.As4(), Port: uint16(values[2].Uint),
		AuthorizedKeys: string(values[3].Bytes()), HostKey: string(values[4].Bytes()),
		MaxSessions: uint8(values[5].Uint), MaxAuthAttempts: uint8(values[6].Uint),
		HandshakeTimeoutMS: uint32(values[7].Uint), IdleTimeoutSec: uint32(values[8].Uint),
	}
	if !ValidPath(policy.AuthorizedKeys) || !ValidPath(policy.HostKey) || !strings.HasPrefix(policy.HostKey, "/state/") || policy.AuthorizedKeys == policy.HostKey ||
		policy.AuthorizedKeys == policy.HostKey+".pub" || policy.AuthorizedKeys == policy.HostKey+".new" || policy.AuthorizedKeys == policy.HostKey+".pub.new" {
		return Policy{}, errors.New("invalid remote key paths")
	}
	for _, item := range []struct {
		index     int
		supported string
		target    *[]string
	}{{9, KEXAlgorithm, &policy.KEX}, {10, CipherAlgorithm, &policy.Ciphers},
		{11, KeyAlgorithm, &policy.HostKeyAlgorithms}, {12, KeyAlgorithm, &policy.UserKeyAlgorithms}} {
		algorithms, err := ParseAlgorithms(string(values[item.index].Bytes()), item.supported)
		if err != nil {
			return Policy{}, err
		}
		*item.target = algorithms
	}
	return policy, nil
}

func validValue(value config.Value) bool {
	switch value.Kind {
	case config.KindBool:
		return value.Uint <= 1 && value.Size == 0
	case config.KindUint:
		return value.Size == 0
	case config.KindText:
		if value.Uint != 0 || int(value.Size) > config.MaxText {
			return false
		}
		for index := 0; index < int(value.Size); index++ {
			if value.Text[index] < 0x20 || value.Text[index] > 0x7e {
				return false
			}
		}
		return true
	}
	return false
}

func bounded(value, low, high uint64) bool { return value >= low && value <= high }

// ValidPath accepts a canonical absolute file path, not a directory or an alias.
func ValidPath(path string) bool {
	if len(path) < 2 || len(path) > MaxPath || path[0] != '/' || path[len(path)-1] == '/' {
		return false
	}
	for _, component := range strings.Split(path[1:], "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, c := range []byte(component) {
			if c < 0x21 || c > 0x7e || c == '\\' {
				return false
			}
		}
	}
	return true
}

// ParseAlgorithms accepts only canonical comma-separated compiled names. The
// caller supplies a category's compiled allowlist, never a user's allowlist.
func ParseAlgorithms(csv string, supported ...string) ([]string, error) {
	if csv == "" || len(csv) > config.MaxText {
		return nil, errors.New("remote algorithm list empty or too long")
	}
	items := strings.Split(csv, ",")
	for index, name := range items {
		known := false
		for _, candidate := range supported {
			if name == candidate {
				known = true
			}
		}
		if !known || name == "" {
			return nil, errors.New("unsupported or malformed remote algorithm")
		}
		for previous := 0; previous < index; previous++ {
			if items[previous] == name {
				return nil, errors.New("duplicate remote algorithm")
			}
		}
	}
	return items, nil
}
