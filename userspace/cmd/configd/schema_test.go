package main

import (
	"goos/userspace/config"
	"goos/userspace/remotepolicy"
	"testing"
)

func TestRemoteSchemaDefaultsAndRights(t *testing.T) {
	var store config.Store
	if status := declareSchema(&store); status != config.StatusOK {
		t.Fatal(status)
	}
	scope, _ := config.MakeScope("system.remote.")
	for _, setting := range []struct {
		name string
		want config.Value
	}{
		{"enabled", config.Bool(false)},
		{"listen-address", mustText("0.0.0.0")},
		{"port", config.Uint(22)},
		{"user", mustText("user")},
		{"authorized-keys", mustText("/home/user/.ssh/authorized_keys")},
		{"host-key", mustText("/state/security/ssh/ssh_host_ed25519_key")},
		{"max-sessions", config.Uint(4)},
		{"max-auth-attempts", config.Uint(3)},
		{"handshake-timeout-ms", config.Uint(15000)},
		{"idle-timeout-sec", config.Uint(0)},
		{"kex", mustText("curve25519-sha256")},
		{"ciphers", mustText("aes128-gcm@openssh.com")},
		{"host-key-algorithms", mustText("ssh-ed25519")},
		{"user-key-algorithms", mustText("ssh-ed25519")},
	} {
		key := []byte("system.remote." + setting.name)
		value, status := store.Get(scope, config.Read, key)
		if status != config.StatusOK || value != setting.want {
			t.Fatalf("%s = %+v, %v", key, value, status)
		}
		if status := store.Set(scope, config.Read|config.Write, key, value); status != config.StatusOK {
			t.Fatalf("%s not writable: %v", key, status)
		}
		wrong := config.Bool(true)
		if value.Kind == config.KindBool {
			wrong = config.Uint(1)
		}
		if status := store.Set(scope, config.Read|config.Write, key, wrong); status != config.StatusTypeMismatch {
			t.Fatalf("%s accepts wrong type: %v", key, status)
		}
	}
	policy, err := remotepolicy.Load(func(key string) (config.Value, config.Status) { return store.Get(scope, config.Read, []byte(key)) })
	if err != nil || policy.Enabled || policy.Port != 22 || policy.User != "user" {
		t.Fatalf("schema policy = %+v, %v", policy, err)
	}
	if _, status := store.Get(scope, config.Read, []byte("user.name")); status != config.StatusNotFound {
		t.Fatal("remote scope sees user setting")
	}
}

func TestSchemaCapacityAndPublishedBuild(t *testing.T) {
	var store config.Store
	if status := declareSchema(&store); status != config.StatusOK {
		t.Fatal(status)
	}
	scope, _ := config.MakeScope("")
	if status := store.Set(scope, config.Read|config.Write, []byte("system.build.name"), mustText("changed")); status != config.StatusReadOnly {
		t.Fatal("build setting writable")
	}
	// Existing 54 declarations plus the 14 remote settings leave room for more.
	for index := 0; index < config.MaxEntries-68; index++ {
		key := []byte{'s', 'p', 'a', 'r', 'e', '.', byte('a' + index/26), byte('a' + index%26)}
		if status := store.Declare(string(key), config.Bool(false), false); status != config.StatusOK {
			t.Fatalf("spare %d: %v", index, status)
		}
	}
	if status := store.Declare("spare.overflow", config.Bool(false), false); status != config.StatusResourceExhausted {
		t.Fatalf("unexpected schema capacity: %v", status)
	}
}
