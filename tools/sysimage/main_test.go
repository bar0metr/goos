package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopyApps(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "host")
	target := filepath.Join(root, "image", "apps")
	if err := os.MkdirAll(filepath.Join(source, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x7f, 'E', 'L', 'F', 0, 1}
	if err := os.WriteFile(filepath.Join(source, "tools", "probe"), want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkAppsSource(source, filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	if err := copyApps(source, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "tools", "probe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("copied file = %q, want %q", got, want)
	}
}

func TestAppsSourceCannotOverlapOutput(t *testing.T) {
	root := t.TempDir()
	for _, pair := range [][2]string{
		{root, filepath.Join(root, "image")},
		{filepath.Join(root, "image"), root},
	} {
		if err := os.MkdirAll(pair[0], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := checkAppsSource(pair[0], pair[1]); err == nil || !strings.Contains(err.Error(), "overlaps output") {
			t.Fatalf("checkAppsSource(%q, %q) = %v", pair[0], pair[1], err)
		}
	}
}

func TestCopyAppsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "host")
	target := filepath.Join(root, "image", "apps")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyApps(source, target); err == nil || !strings.Contains(err.Error(), "not a regular file or directory") {
		t.Fatalf("copyApps() = %v", err)
	}
}

func validationCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1893456000, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func TestCABundleInput(t *testing.T) {
	if _, err := readCABundle(""); err == nil {
		t.Fatal("missing explicit CA input accepted")
	}
	production, err := readCABundle("../../security/ca-certificates.pem")
	if err != nil || len(production) == 0 {
		t.Fatalf("production bundle: %v", err)
	}
	if _, err := readCABundle("../../security/ca-source.pem"); err == nil {
		t.Fatal("raw titled bundle accepted as canonical")
	}
	root := validationCA(t)
	path := filepath.Join(t.TempDir(), "fixture.pem")
	for _, data := range [][]byte{nil, []byte("broken"), append([]byte("# comment\n"), root...), bytes.Repeat(root, 257), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: make([]byte, (32<<10)+1)})} {
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCABundle(path); err == nil {
			t.Fatal("invalid CA bundle accepted")
		}
	}
	if err := os.WriteFile(path, root, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := readCABundle(path)
	if err != nil || !bytes.Equal(got, root) {
		t.Fatalf("explicit fixture root: %v", err)
	}
}
func TestSysimageMainProcess(t *testing.T) {
	if os.Getenv("SYSIMAGE_TEST_PROCESS") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("SYSIMAGE_TEST_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"sysimage"}, args...)
	main()
	os.Exit(0)
}
func TestSystemCAInstallAndFailBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	var args []string
	for _, name := range []string{"shell", "config", "log", "netd", "virtio-net", "resolved", "remoted", "remoted-worker"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	commands := filepath.Join(root, "commands")
	if err := os.Mkdir(commands, 0755); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, "index")
	if err := os.WriteFile(index, nil, 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "image")
	args = append(args, commands, index, output)
	fixture := filepath.Join(root, "validation-roots.pem")
	want := validationCA(t)
	if err := os.WriteFile(fixture, want, 0644); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(args)
	invoke := func(input string) ([]byte, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSysimageMainProcess$")
		cmd.Env = append(os.Environ(), "SYSIMAGE_TEST_PROCESS=1", "SYSIMAGE_TEST_ARGS="+string(encoded), "SYSTEM_CA_BUNDLE="+input, "SYSTEM_TRUST_UPDATE_KEY=../../security/trust-update-key.pub", "SYSTEM_TRUST_UPDATE_URL=https://updates.example.test/trust", "LINUX_APPS_DIR=", "LINUX_EXTRA=")
		return cmd.CombinedOutput()
	}
	if log, err := invoke(fixture); err != nil {
		t.Fatalf("sysimage: %v\n%s", err, log)
	}
	for _, name := range []string{"shell", "config", "log", "netd", "virtio-net", "resolved", "remoted", "remoted-worker"} {
		got, err := os.ReadFile(filepath.Join(output, "system/services", name))
		if err != nil || string(got) != name {
			t.Fatalf("installed service %s: data=%q err=%v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "state/security/ssh/ssh_host_ed25519_key")); !os.IsNotExist(err) {
		t.Fatalf("image contains a host identity: %v", err)
	}
	installed := filepath.Join(output, "system/security/ca-certificates.pem")
	got, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("installed CA mismatch: %v", err)
	}
	info, err := os.Stat(installed)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("installed CA mode: %v", err)
	}
	for _, input := range []string{"", filepath.Join(root, "missing.pem")} {
		if _, err := invoke(input); err == nil {
			t.Fatal("invalid CA selection accepted")
		}
		got, err := os.ReadFile(installed)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("CA validation failure removed existing output")
		}
	}
}

func TestTrustUpdateInputs(t *testing.T) {
	keyPath := "../../security/trust-update-key.pub"
	key, url, err := readTrustInputs(keyPath, "https://updates.example.test/trust")
	if err != nil || len(key) != 32 || string(url) != "https://updates.example.test/trust\n" {
		t.Fatalf("inputs: key=%d url=%q err=%v", len(key), url, err)
	}
	for _, bad := range []string{"http://updates.example.test/trust", "https://host/path?x=1", "https://host/path\n"} {
		if _, _, err := readTrustInputs(keyPath, bad); err == nil {
			t.Fatalf("accepted URL %q", bad)
		}
	}
	if _, _, err := readTrustInputs("", ""); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, _, err := readTrustInputs(keyPath, ""); err != nil {
		t.Fatal("empty disabled endpoint rejected", err)
	}
	path := filepath.Join(t.TempDir(), "key")
	for _, data := range [][]byte{nil, make([]byte, 32), make([]byte, 33), []byte("not a public key")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readTrustInputs(path, ""); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
