package remotepolicy

import (
	"goos/userspace/config"
	"reflect"
	"strings"
	"testing"
)

func defaultValues(t *testing.T) map[string]config.Value {
	t.Helper()
	var store config.Store
	if status := DeclareDefaults(&store); status != config.StatusOK {
		t.Fatal(status)
	}
	scope, _ := config.MakeScope(Prefix)
	values := make(map[string]config.Value)
	for _, item := range defaults() {
		value, status := store.Get(scope, config.Read, []byte(Prefix+item.name))
		if status != config.StatusOK {
			t.Fatal(status)
		}
		values[Prefix+item.name] = value
	}
	return values
}

func getter(values map[string]config.Value) Getter {
	return func(key string) (config.Value, config.Status) {
		value, ok := values[key]
		if !ok {
			return config.Value{}, config.StatusNotFound
		}
		return value, config.StatusOK
	}
}

func TestLoadDefaults(t *testing.T) {
	policy, err := Load(getter(defaultValues(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := Policy{
		Port: 22, User: "user", AuthorizedKeys: "/home/user/.ssh/authorized_keys", HostKey: "/state/security/ssh/ssh_host_ed25519_key",
		MaxSessions: 4, MaxAuthAttempts: 3, HandshakeTimeoutMS: 15000,
		KEX: []string{KEXAlgorithm}, Ciphers: []string{CipherAlgorithm},
		HostKeyAlgorithms: []string{KeyAlgorithm}, UserKeyAlgorithms: []string{KeyAlgorithm},
	}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
}

func TestLoadRejectsIncompleteSnapshot(t *testing.T) {
	for _, item := range defaults() {
		t.Run(item.name, func(t *testing.T) {
			values := defaultValues(t)
			delete(values, Prefix+item.name)
			policy, err := Load(getter(values))
			if err == nil || !reflect.DeepEqual(policy, Policy{}) {
				t.Fatalf("missing setting returned %+v, %v", policy, err)
			}
		})
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("nil getter accepted")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value config.Value
	}{
		{"enabled", config.Uint(1)}, {"enabled", config.Value{Kind: config.KindBool, Uint: 2}},
		{"port", config.Uint(0)}, {"port", config.Uint(65536)}, {"port", config.Value{Kind: config.KindUint, Size: 1}},
		{"max-sessions", config.Uint(0)}, {"max-sessions", config.Uint(5)},
		{"max-auth-attempts", config.Uint(0)}, {"max-auth-attempts", config.Uint(11)},
		{"handshake-timeout-ms", config.Uint(999)}, {"handshake-timeout-ms", config.Uint(120001)},
		{"idle-timeout-sec", config.Uint(86401)},
		{"listen-address", text("127.0.0.01")}, {"listen-address", text("::ffff:127.0.0.1")},
		{"listen-address", text("::1")}, {"listen-address", text("example.com")},
		{"listen-address", text("1.2.3.4:22")}, {"listen-address", text(" 1.2.3.4")},
		{"authorized-keys", text("relative")}, {"authorized-keys", text("/a/../b")},
		{"host-key", text("/a//b")}, {"host-key", text("/state/security/ssh/ssh_host_ed25519_key/")},
		{"host-key", text("/home/user/.ssh/authorized_keys")},
		{"host-key", text("/home/user/private-key")}, {"host-key", text("/run/private-key")},
		{"authorized-keys", text("/state/security/ssh/ssh_host_ed25519_key.new")},
		{"authorized-keys", text("/state/security/ssh/ssh_host_ed25519_key.pub.new")},
		{"kex", text("curve25519-sha256@libssh.org")}, {"ciphers", text("aes128-ctr")},
		{"host-key-algorithms", text("ssh-rsa")}, {"user-key-algorithms", text("ssh-ed25519,ssh-ed25519")},
		{"user-key-algorithms", config.Value{Kind: config.KindText, Size: 65}},
		{"user-key-algorithms", config.Value{Kind: config.KindText, Size: 1, Uint: 1}},
		{"user-key-algorithms", config.Value{Kind: config.KindText, Size: 1}},
	}
	for _, test := range tests {
		t.Run(test.name+"/"+string(test.value.Text[:min(int(test.value.Size), config.MaxText)]), func(t *testing.T) {
			values := defaultValues(t)
			values[Prefix+test.name] = test.value
			policy, err := Load(getter(values))
			if err == nil || !reflect.DeepEqual(policy, Policy{}) {
				t.Fatalf("invalid setting returned %+v, %v", policy, err)
			}
		})
	}
}

func TestLoadBoundaryValues(t *testing.T) {
	for _, bounds := range []struct {
		name      string
		low, high uint64
	}{
		{"port", 1, 65535}, {"max-sessions", 1, 4}, {"max-auth-attempts", 1, 10},
		{"handshake-timeout-ms", 1000, 120000}, {"idle-timeout-sec", 0, 86400},
	} {
		for _, value := range []uint64{bounds.low, bounds.high} {
			values := defaultValues(t)
			values[Prefix+bounds.name] = config.Uint(value)
			values[Prefix+"enabled"] = config.Bool(true)
			values[Prefix+"listen-address"] = text("192.0.2.1")
			values[Prefix+"authorized-keys"] = text("/" + strings.Repeat("a", MaxPath-1))
			policy, err := Load(getter(values))
			if err != nil || !policy.Enabled || policy.ListenAddress != [4]byte{192, 0, 2, 1} {
				t.Fatalf("boundary %s=%d: %+v, %v", bounds.name, value, policy, err)
			}
		}
	}
}

func TestValidPath(t *testing.T) {
	for _, path := range []string{"/a", "/state/security/ssh/key", "/" + strings.Repeat("a", MaxPath-1)} {
		if !ValidPath(path) {
			t.Errorf("valid path %q rejected", path)
		}
	}
	for _, path := range []string{"", "/", "relative", "//a", "/a//b", "/a/.", "/a/..", "/a/../b", "/a/", "/a\\b", "/a\x00b", "/a b", "/" + strings.Repeat("a", MaxPath)} {
		if ValidPath(path) {
			t.Errorf("invalid path %q accepted", path)
		}
	}
}

func TestAlgorithmLists(t *testing.T) {
	for _, supported := range []string{KEXAlgorithm, KeyAlgorithm, CipherAlgorithm} {
		items, err := ParseAlgorithms(supported, supported)
		if err != nil || !reflect.DeepEqual(items, []string{supported}) {
			t.Fatalf("canonical list rejected: %v, %v", items, err)
		}
		for _, malformed := range []string{"", ",", supported + ",", "," + supported, supported + ",," + supported,
			supported + "," + supported, " " + supported, supported + " ", strings.ToUpper(supported),
			supported + ",unknown", "unknown," + supported, supported + "\t", strings.Repeat("a", config.MaxText+1)} {
			if _, err := ParseAlgorithms(malformed, supported); err == nil {
				t.Errorf("invalid list %q accepted", malformed)
			}
		}
	}
}

func TestRemoteLoginLabelAndIndependentKeyPath(t *testing.T) {
	for _, user := range []string{"u", "Admin_0-9", strings.Repeat("a", MaxUser)} {
		values := defaultValues(t)
		values[Prefix+"user"] = text(user)
		values[Prefix+"authorized-keys"] = text("/home/user/.ssh/custom_keys")
		policy, err := Load(getter(values))
		if err != nil || policy.User != user || policy.AuthorizedKeys != "/home/user/.ssh/custom_keys" {
			t.Fatal(policy, err)
		}
	}
	for _, user := range []string{"", strings.Repeat("a", MaxUser+1), "user.name", "user/name", "user name", "user!", "юзер", "user\n"} {
		values := defaultValues(t)
		values[Prefix+"user"] = text(user)
		if ValidUser(user) {
			t.Fatal("invalid user", user)
		}
		if _, err := Load(getter(values)); err == nil {
			t.Fatal("invalid candidate", user)
		}
	}
}
