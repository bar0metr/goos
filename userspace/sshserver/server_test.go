package sshserver

import (
	"crypto/ed25519"
	"net"
	"testing"
)

func validConfig() Config {
	return Config{HostKey: make(ed25519.PrivateKey, 64), Username: "user", AuthorizedKeys: []ed25519.PublicKey{make(ed25519.PublicKey, 32)}, MaxAuthAttempts: 5}
}
func TestConfigBounds(t *testing.T) {
	tests := []func(*Config){func(c *Config) { c.Username = "root" }, func(c *Config) { c.HostKey = nil }, func(c *Config) { c.AuthorizedKeys = nil }, func(c *Config) { c.AuthorizedKeys = make([]ed25519.PublicKey, 65) }, func(c *Config) { c.AuthorizedKeys[0] = make([]byte, 31) }, func(c *Config) { c.Banner = string(make([]byte, 1025)) }, func(c *Config) { c.MaxAuthAttempts = 0 }, func(c *Config) { c.MaxAuthAttempts = 11 }}
	for i, change := range tests {
		c := validConfig()
		change(&c)
		a, b := net.Pipe()
		_, err := New(a, c)
		a.Close()
		b.Close()
		if err != ErrConfig {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	if _, err := New(nil, validConfig()); err != ErrConfig {
		t.Fatal(err)
	}
}
