package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDotenv(t *testing.T) {
	in := `
# comment
PVE_HOST=pve.example.invalid
PVE_PORT = 8006
QUOTED="a b # not a comment"
SINGLE='x=y'
TRAILING=value # comment
EMPTY=
`
	got, err := ParseDotenv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PVE_HOST": "pve.example.invalid", "PVE_PORT": "8006", "QUOTED": "a b # not a comment",
		"SINGLE": "x=y", "TRAILING": "value", "EMPTY": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d", len(got), len(want))
	}
}

func TestParseDotenvRejectsMalformed(t *testing.T) {
	for _, in := range []string{"NOEQUALS", "=value", "1KEY=x", "KEY=\"open", "KEY='a' b", "BAD-KEY=x"} {
		if _, err := ParseDotenv(strings.NewReader(in)); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestParseDotenvDoesNotExpand(t *testing.T) {
	got, err := ParseDotenv(strings.NewReader("A=$(whoami) ${HOME} `id`"))
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "$(whoami) ${HOME} `id`" {
		t.Errorf("A = %q", got["A"])
	}
}

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("PVE_HOST=from-file\nPVE_NODE=file-node\nPVE_CORES=2\n"), 0o600)
	cfg, err := LoadConfig(dir, env, lookupFrom(map[string]string{"PVE_NODE": "env-node"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "from-file" {
		t.Errorf("Host = %q: .env must beat the default", cfg.Host)
	}
	if cfg.Node != "env-node" {
		t.Errorf("Node = %q: the environment must beat .env", cfg.Node)
	}
	if cfg.Cores != 2 || cfg.Memory != 1024 || cfg.CPU != "host" || cfg.Timeout != 180*time.Second {
		t.Errorf("defaults or file values wrong: %+v", cfg)
	}
	if cfg.SSHHost != "from-file" {
		t.Errorf("SSHHost = %q, want PVE_HOST when unset", cfg.SSHHost)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{"PVE_PORT": "x", "PVE_TLS_INSECURE": "maybe", "PVE_TIMEOUT": "-1s", "PVE_SSH_SUDO": "always"} {
		_, err := LoadConfig(t.TempDir(), "", lookupFrom(map[string]string{k: v}))
		if err == nil {
			t.Errorf("%s=%s: no error", k, v)
		}
	}
}

func TestParseBool(t *testing.T) {
	for in, want := range map[string]bool{"1": true, "yes": true, "TRUE": true, "0": false, "": false, "off": false} {
		got, err := parseBool(in)
		if err != nil || got != want {
			t.Errorf("parseBool(%q) = %v, %v", in, got, err)
		}
	}
}

func TestValidateNamesMissingSettings(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir(), "", lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("no error for an empty configuration")
	}
	for _, key := range []string{"PVE_HOST", "PVE_TOKEN_SECRET", "PVE_POOL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not name %s: %v", key, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	const secret = "c0ffee-secret"
	cfg := &Config{TokenSecret: Secret(secret), TokenID: "u@pve!t"}
	for _, text := range []string{
		fmt.Sprint(cfg.TokenSecret), fmt.Sprintf("%v %+v %#v %s %q", cfg, cfg, cfg, cfg.TokenSecret, cfg.TokenSecret),
		string(jsonIndent(cfg)),
	} {
		if strings.Contains(text, secret) {
			t.Errorf("secret leaked: %s", text)
		}
	}
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), secret) {
		t.Errorf("secret in JSON: %s", b)
	}
}

func TestSudoDefault(t *testing.T) {
	for _, c := range []struct {
		user, mode string
		want       bool
	}{{"root", "auto", false}, {"alice", "auto", true}, {"alice", "0", false}, {"root", "1", true}} {
		cfg := &Config{SSHUser: c.user, SSHSudo: c.mode}
		if cfg.Sudo() != c.want {
			t.Errorf("user %s mode %s: Sudo = %v", c.user, c.mode, cfg.Sudo())
		}
	}
}
