package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Secret holds a credential. Every way of printing or encoding it yields a
// placeholder, so a Config can be logged or marshalled without leaking it.
type Secret string

const redacted = "[redacted]"

func (Secret) String() string                  { return redacted }
func (Secret) GoString() string                { return redacted }
func (Secret) MarshalJSON() ([]byte, error)    { return []byte(`"` + redacted + `"`), nil }
func (s Secret) Format(f fmt.State, verb rune) { io.WriteString(f, redacted) }

// Config is the lab configuration. Precedence, highest first: command-line
// flag, process environment, .env, built-in default. The token secret has no
// flag.
type Config struct {
	RepoRoot string `json:"-"`
	EnvFile  string `json:"-"`

	Host        string `json:"host"`
	Port        int    `json:"port"`
	Node        string `json:"node"`
	TokenID     string `json:"token_id"`
	TokenSecret Secret `json:"-"`
	CAFile      string `json:"ca_file,omitempty"`
	TLSInsecure bool   `json:"tls_insecure"`

	SSHHost string `json:"ssh_host"`
	SSHPort int    `json:"ssh_port"`
	SSHUser string `json:"ssh_user"`
	SSHKey  string `json:"-"`
	// SSHSudo is "auto" (sudo -n when SSHUser is not root), "1", or "0".
	SSHSudo string `json:"ssh_sudo"`

	Pool        string `json:"pool"`
	DiskStorage string `json:"disk_storage"`
	ISOStorage  string `json:"iso_storage"`
	EFIStorage  string `json:"efi_storage"`

	Machine string `json:"machine"`
	BIOS    string `json:"bios"`
	CPU     string `json:"cpu"`
	Sockets int    `json:"sockets"`
	Cores   int    `json:"cores"`
	Memory  int    `json:"memory_mib"`
	// NetBridge is the bridge the VM's virtio network function joins; empty
	// leaves the VM without one.
	NetBridge string `json:"net_bridge"`

	Timeout     time.Duration `json:"timeout"`
	Keep        bool          `json:"keep"`
	MaxParallel int           `json:"max_parallel"`
}

type setting struct {
	key  string
	def  string
	set  func(*Config, string) error
	need bool
}

func text(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *field(c) = v; return nil }
}

func number(key string, field func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: %q is not a non-negative integer", key, v)
		}
		*field(c) = n
		return nil
	}
}

func boolean(key string, field func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*field(c) = b
		return nil
	}
}

var settings = []setting{
	{"PVE_HOST", "", text(func(c *Config) *string { return &c.Host }), true},
	{"PVE_PORT", "8006", number("PVE_PORT", func(c *Config) *int { return &c.Port }), true},
	{"PVE_NODE", "", text(func(c *Config) *string { return &c.Node }), true},
	{"PVE_TOKEN_ID", "", text(func(c *Config) *string { return &c.TokenID }), true},
	{"PVE_TOKEN_SECRET", "", func(c *Config, v string) error { c.TokenSecret = Secret(v); return nil }, true},
	{"PVE_CA_FILE", "", text(func(c *Config) *string { return &c.CAFile }), false},
	{"PVE_TLS_INSECURE", "0", boolean("PVE_TLS_INSECURE", func(c *Config) *bool { return &c.TLSInsecure }), false},
	{"PVE_SSH_HOST", "", text(func(c *Config) *string { return &c.SSHHost }), false},
	{"PVE_SSH_PORT", "22", number("PVE_SSH_PORT", func(c *Config) *int { return &c.SSHPort }), true},
	{"PVE_SSH_USER", "root", text(func(c *Config) *string { return &c.SSHUser }), true},
	{"PVE_SSH_KEY", "", text(func(c *Config) *string { return &c.SSHKey }), false},
	{"PVE_SSH_SUDO", "auto", func(c *Config, v string) error {
		switch v {
		case "auto", "0", "1":
			c.SSHSudo = v
			return nil
		}
		return fmt.Errorf("PVE_SSH_SUDO: %q is not auto, 0, or 1", v)
	}, false},
	{"PVE_POOL", "", text(func(c *Config) *string { return &c.Pool }), true},
	{"PVE_DISK_STORAGE", "", text(func(c *Config) *string { return &c.DiskStorage }), true},
	{"PVE_ISO_STORAGE", "", text(func(c *Config) *string { return &c.ISOStorage }), true},
	{"PVE_EFI_STORAGE", "", text(func(c *Config) *string { return &c.EFIStorage }), true},
	{"PVE_MACHINE", "q35", text(func(c *Config) *string { return &c.Machine }), true},
	{"PVE_BIOS", "ovmf", text(func(c *Config) *string { return &c.BIOS }), true},
	{"PVE_CPU", "host", text(func(c *Config) *string { return &c.CPU }), true},
	{"PVE_SOCKETS", "1", number("PVE_SOCKETS", func(c *Config) *int { return &c.Sockets }), true},
	{"PVE_CORES", "1", number("PVE_CORES", func(c *Config) *int { return &c.Cores }), true},
	{"PVE_NET_BRIDGE", "vmbr0", text(func(c *Config) *string { return &c.NetBridge }), false},
	{"PVE_MEMORY", "1024", number("PVE_MEMORY", func(c *Config) *int { return &c.Memory }), true},
	{"PVE_TIMEOUT", "180s", func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("PVE_TIMEOUT: %q is not a positive duration", v)
		}
		c.Timeout = d
		return nil
	}, true},
	{"PVE_KEEP_VM", "0", boolean("PVE_KEEP_VM", func(c *Config) *bool { return &c.Keep }), false},
	{"PVE_MAX_PARALLEL", "1", number("PVE_MAX_PARALLEL", func(c *Config) *int { return &c.MaxParallel }), true},
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", v)
}

// LoadConfig merges the environment, the dotenv file, and the defaults.
// lookup is os.LookupEnv outside tests. Flags are applied by the caller
// afterwards with Config.Set.
func LoadConfig(root, envFile string, lookup func(string) (string, bool)) (*Config, error) {
	file := map[string]string{}
	if envFile == "" {
		envFile = filepath.Join(root, ".env")
	}
	if f, err := os.Open(envFile); err == nil {
		file, err = ParseDotenv(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c := &Config{RepoRoot: root, EnvFile: envFile}
	for _, s := range settings {
		v, ok := lookup(s.key)
		if !ok {
			v, ok = file[s.key]
		}
		if !ok {
			v = s.def
		}
		if err := s.set(c, v); err != nil {
			return nil, err
		}
	}
	if c.SSHHost == "" {
		c.SSHHost = c.Host
	}
	c.SSHKey = expandHome(c.SSHKey)
	c.CAFile = expandHome(c.CAFile)
	return c, nil
}

// Validate reports every required setting that is empty or out of range.
func (c *Config) Validate() error {
	var missing []string
	for _, s := range settings {
		if !s.need {
			continue
		}
		if c.value(s.key) == "" {
			missing = append(missing, s.key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing configuration: %s (see .env.example)", strings.Join(missing, ", "))
	}
	if c.Sockets < 1 || c.Cores < 1 || c.Memory < 64 {
		return fmt.Errorf("sockets and cores must be at least 1 and memory at least 64 MiB")
	}
	if c.MaxParallel < 1 {
		return fmt.Errorf("PVE_MAX_PARALLEL must be at least 1")
	}
	if c.SSHKey != "" {
		if _, err := os.Stat(c.SSHKey); err != nil {
			return fmt.Errorf("PVE_SSH_KEY: %w", err)
		}
	}
	return nil
}

func (c *Config) value(key string) string {
	switch key {
	case "PVE_HOST":
		return c.Host
	case "PVE_NODE":
		return c.Node
	case "PVE_TOKEN_ID":
		return c.TokenID
	case "PVE_TOKEN_SECRET":
		return string(c.TokenSecret)
	case "PVE_SSH_USER":
		return c.SSHUser
	case "PVE_POOL":
		return c.Pool
	case "PVE_DISK_STORAGE":
		return c.DiskStorage
	case "PVE_ISO_STORAGE":
		return c.ISOStorage
	case "PVE_EFI_STORAGE":
		return c.EFIStorage
	case "PVE_MACHINE":
		return c.Machine
	case "PVE_BIOS":
		return c.BIOS
	case "PVE_CPU":
		return c.CPU
	}
	return "set" // numeric settings are checked by Validate's range test
}

// Sudo reports whether remote Proxmox commands run through sudo -n.
func (c *Config) Sudo() bool {
	switch c.SSHSudo {
	case "1":
		return true
	case "0":
		return false
	}
	return c.SSHUser != "root"
}

// ParseDotenv reads KEY=value lines. It accepts blank lines, # comments, and
// single- or double-quoted values; it never expands or evaluates anything.
func ParseDotenv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, value, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || !validKey(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", line)
		}
		value = strings.TrimSpace(value)
		if value != "" && (value[0] == '"' || value[0] == '\'') {
			quote := value[0]
			end := strings.IndexByte(value[1:], quote)
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated quote", line)
			}
			rest := strings.TrimSpace(value[end+2:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("line %d: text after closing quote", line)
			}
			value = value[1 : end+1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		out[key] = value
	}
	return out, scanner.Err()
}

func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// FindRepoRoot walks up from dir to the GoOS module root.
func FindRepoRoot(dir string) (string, error) {
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(data), "module goos\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside the GoOS repository")
		}
		dir = parent
	}
}

// CheckEnvIgnored refuses a .env that git would commit.
func CheckEnvIgnored(root, envFile string) error {
	if _, err := os.Stat(envFile); err != nil {
		return nil
	}
	rel, err := filepath.Rel(root, envFile)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // outside the repository: git never sees it
	}
	cmd := exec.Command("git", "check-ignore", "-q", "--", rel)
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s holds credentials but git does not ignore it; add /%s to .gitignore", rel, rel)
	}
	return nil
}
