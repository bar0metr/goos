package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPublic(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPinHostKey(t *testing.T) {
	key := testPublic(t)
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	fingerprint := ssh.FingerprintSHA256(key)
	scan := "# banner\nignored-scan-address " + public + "\n"
	for _, port := range []int{22, 2222} {
		known, err := pinHostKey(scan, fingerprint, "10.0.0.42", port)
		if err != nil {
			t.Fatal(err)
		}
		want := "10.0.0.42"
		if port != 22 {
			want = "[10.0.0.42]:2222"
		}
		if string(known) != want+" "+public+"\n" {
			t.Fatalf("pin = %q", known)
		}
	}
	for _, bad := range []string{"", scan + scan, "host ssh-rsa AAAA\n", "host ssh-ed25519 broken\n", "host " + public + " extra\n"} {
		if _, err := pinHostKey(bad, fingerprint, "10.0.0.42", 22); err == nil {
			t.Fatalf("accepted scan %q", bad)
		}
	}
	if _, err := pinHostKey(scan, "SHA256:wrong", "10.0.0.42", 22); err == nil {
		t.Fatal("accepted mismatched trusted fingerprint")
	}
}

func TestGuestSSHArgumentsAlwaysPin(t *testing.T) {
	client := guestSSH{Host: "10.0.0.42", Port: 2222, User: "user", Key: "/tmp/disposable", KnownHosts: "/tmp/pinned"}
	for _, pty := range []bool{false, true} {
		args := client.args(pty, "uname")
		for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=/tmp/pinned", "GlobalKnownHostsFile=/dev/null", "IdentityAgent=none", "IdentitiesOnly=yes", "UpdateHostKeys=no"} {
			if !hasArg(args, want) {
				t.Errorf("missing %s from %v", want, args)
			}
		}
		if args[0] != "-F" || args[1] != "/dev/null" || hasArg(args, "StrictHostKeyChecking=no") || hasArg(args, "StrictHostKeyChecking=accept-new") {
			t.Fatalf("unsafe args %v", args)
		}
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestClientKeyDisposable(t *testing.T) {
	path, public, fingerprint, err := makeClientKey(t.TempDir(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private permissions = %v, %v", info, err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(encoded)
	if err != nil || ssh.FingerprintSHA256(signer.PublicKey()) != fingerprint {
		t.Fatalf("private/public mismatch: %v", err)
	}
	key, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(public))
	if err != nil || key.Type() != "ssh-ed25519" || comment != "pvelab-primary" || ssh.FingerprintSHA256(key) != fingerprint {
		t.Fatalf("public = %q, %v", public, err)
	}
}

func TestRemoteParsers(t *testing.T) {
	good := "state: ready\nlink: up\naddress: 10.0.0.42/24\n"
	if ip, err := reachableGuestAddress(good); err != nil || ip != "10.0.0.42" {
		t.Fatalf("%q %v", ip, err)
	}
	for _, address := range []string{"none", "0.0.0.0", "127.0.0.1", "169.254.1.1", "224.1.1.1", "::1", "bad;reboot"} {
		if _, err := reachableGuestAddress("state: ready\nlink: up\naddress: " + address); err == nil {
			t.Errorf("accepted address %q", address)
		}
	}
	if _, err := reachableGuestAddress("state: configuring\nlink: up\naddress: 10.0.0.42"); err == nil {
		t.Error("accepted unready NIC")
	}
	if !validUTCDate("2026-10-11T12:00:00Z\n") || validUTCDate("local noon") {
		t.Error("UTC date validation")
	}
	record := "[2026-10-11T12:00:00Z] info kernel: event\n"
	if !validLogSnapshot(record) || validLogSnapshot("") || validLogSnapshot(strings.Repeat(record, 6)) {
		t.Error("snapshot bounds")
	}
	if pid, err := foregroundPID("42 native running 5 8 /bin/gfm\n", "gfm"); err != nil || pid != 42 {
		t.Fatalf("pid %d %v", pid, err)
	}
	if _, err := foregroundPID("42 native running 5 8 /bin/gfm\n43 native running 5 8 /bin/gfm\n", "gfm"); err == nil {
		t.Error("accepted multiple gfm processes")
	}
}

func TestRemoteStatusFieldsAcceptsReloadOutputOnly(t *testing.T) {
	// `remote reload` emits its own status. The serial setup command must not
	// append `remote status`, because this parser intentionally rejects two
	// complete status lines rather than silently choosing one.
	reload := "CONFIG_SET_OK\nservice=running listener=listening settings=configured enabled=true user=admin fingerprint=SHA256:test bind=0.0.0.0:22\n"
	if _, err := remoteStatusFields(reload); err != nil {
		t.Fatalf("reload output rejected: %v", err)
	}
	if _, err := remoteStatusFields(reload + "service=running listener=listening settings=configured enabled=true user=admin fingerprint=SHA256:test bind=0.0.0.0:22\n"); err == nil {
		t.Fatal("duplicate remote status lines accepted")
	}
}

func TestRemoteAssertionsFailClosed(t *testing.T) {
	g := &Guest{logf: func(string, ...any) {}}
	for _, failure := range []struct {
		err    error
		status string
		class  Class
	}{
		{errors.New("bad output"), "fail", ClassScenario},
		{failf(ClassInfra, "ssh", "unreachable"), "blocked", ClassInfra},
	} {
		r := &RemoteAdminReport{Assertions: []RemoteAssertion{{Name: "check", Status: "not-run"}, {Name: "later", Status: "not-run"}}}
		err := observeRemoteCheck(g, r, "check", func() (string, error) { return "observed failure", failure.err })
		if err == nil || classOf(err) != failure.class || r.Assertions[0].Status != failure.status || r.Assertions[1].Status != "not-run" || len(g.checks) != 0 {
			t.Fatalf("false pass: %v %+v", err, r)
		}
	}
}

// scriptConsole and mockTerminal have no goroutines, subprocesses, or sockets.
// The full scenario test asserts orchestration and evidence, not guest behavior.
type scriptConsole struct {
	pending []byte
	reply   func(string) string
	closed  bool
}

func (c *scriptConsole) ReadUntil(ctx context.Context, marker []byte) ([]byte, error) {
	_, out, err := c.ReadUntilAny(ctx, marker)
	return out, err
}
func (c *scriptConsole) ReadUntilAny(ctx context.Context, markers ...[]byte) (int, []byte, error) {
	if err := ctx.Err(); err != nil {
		return -1, nil, err
	}
	for i, marker := range markers {
		if at := bytes.Index(c.pending, marker); at >= 0 {
			end := at + len(marker)
			out := append([]byte(nil), c.pending[:end]...)
			c.pending = c.pending[end:]
			return i, out, nil
		}
	}
	return -1, nil, context.DeadlineExceeded
}
func (c *scriptConsole) Drain() []byte { out := c.pending; c.pending = nil; return out }
func (c *scriptConsole) Tail(n int) []byte {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	return c.pending[len(c.pending)-n:]
}
func (c *scriptConsole) Write(data []byte) error {
	return c.WriteLine(strings.TrimSuffix(string(data), "\n"))
}
func (c *scriptConsole) WriteLine(command string) error {
	c.pending = append(c.pending, []byte(command+"\n"+c.reply(command)+"\n"+shellPrompt)...)
	return nil
}
func (c *scriptConsole) Close() error { c.closed = true; return nil }

type mockTerminal struct {
	*scriptConsole
	sizes      [][2]int
	following  bool
	exited     bool
	fixedWidth bool
}

func (m *mockTerminal) Resize(columns, rows int) error {
	m.sizes = append(m.sizes, [2]int{columns, rows})
	if m.fixedWidth {
		columns = 80
	}
	m.pending = append(m.pending, []byte(fmt.Sprintf("\x1b[%d;1H%sF10 Quit", rows, strings.Repeat(" ", gfmQuitColumn(columns)-8)))...)
	return nil
}
func (m *mockTerminal) WriteLine(command string) error {
	if command == "log -f -n 0" {
		m.following = true
		m.pending = append(m.pending, []byte(command+"\n")...)
		return nil
	}
	if command == "gfm" {
		m.pending = append(m.pending, []byte("\x1b[24;1H"+strings.Repeat(" ", gfmQuitColumn(80)-8)+"F10 Quit")...)
		return nil
	}
	if command == "exit" {
		m.exited = true
		return nil
	}
	return errors.New("unexpected terminal command " + command)
}
func (m *mockTerminal) Write(data []byte) error {
	if bytes.Equal(data, []byte{3}) {
		m.following = false
		m.pending = append(m.pending, []byte("\n"+shellPrompt)...)
		return nil
	}
	if string(data) == "\x1b[21~" {
		m.pending = append(m.pending, []byte("\n"+shellPrompt)...)
		return nil
	}
	return errors.New("unexpected terminal input")
}
func (m *mockTerminal) Wait(context.Context) (commandResult, error) {
	if !m.exited {
		return commandResult{Status: -1}, errors.New("not exited")
	}
	return commandResult{}, nil
}

type scenarioMock struct {
	t                   *testing.T
	s                   *Session
	terminal            *mockTerminal
	fingerprint, public string
	keys                map[string]bool
	hostname, power     string
	events              []string
	mismatch            bool
	sshCalls            int
	pid                 int
	replacePID          bool
	generation          int
	dropFollowEvents    int
	noReboot            bool
	privateLeak         string
	wrongWidth          bool
}

func newScenarioMock(t *testing.T) *scenarioMock {
	t.Helper()
	key := testPublic(t)
	m := &scenarioMock{t: t, keys: map[string]bool{}, fingerprint: ssh.FingerprintSHA256(key), public: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), power: "running", pid: 42, generation: 1}
	con := &scriptConsole{reply: m.serialReply}
	dir := t.TempDir()
	for _, name := range []string{"serial.log", "state.json", "artifacts.json", "vm-config.json", "host-info.json", "guest-terminal.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("mock evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.terminal = &mockTerminal{scriptConsole: &scriptConsole{pending: []byte(shellPrompt)}}
	m.s = &Session{
		Guest: &Guest{con: con, logf: func(string, ...any) {}}, Report: &Report{RunID: "20261011T120000Z-01234567", VM: VMInfo{NetBridge: "vmbr0"}},
		EvidenceDir: dir, KeepRunning: true, HostRunner: m.run, HostPTYRunner: m.run,
		PowerState: func(context.Context) (string, error) {
			m.events = append(m.events, "state:"+m.power)
			return m.power, nil
		},
		ReconnectSerial: func(context.Context) error { m.events = append(m.events, "serial-reconnect"); return nil },
		StartRetained: func(context.Context) error {
			m.events = append(m.events, "start-retained")
			m.power = "running"
			return nil
		},
		OpenTerminal: func(context.Context, guestSSH, string) (remoteTerminal, error) { return m.terminal, nil },
	}
	m.s.Report.Git = GitInfo{SHA: strings.Repeat("a", 40)}
	m.s.Report.Proxmox = ProxmoxInfo{VMID: 42, Node: "node", Pool: "pool"}
	m.s.Report.Artifacts.ISOSHA256 = strings.Repeat("b", 64)
	m.s.Report.Artifacts.DiskSHA256 = strings.Repeat("c", 64)
	metadata := map[string]any{
		"state.json":     State{RunID: m.s.Report.RunID, GitSHA: m.s.Report.Git.SHA, VMID: 42, Node: "node"},
		"artifacts.json": Artifacts{ISO: Artifact{SHA256: m.s.Report.Artifacts.ISOSHA256, Size: 1}, Disk: Artifact{SHA256: m.s.Report.Artifacts.DiskSHA256, Size: 1}},
		"vm-config.json": map[string]string{"name": "mock-vm"},
		"host-info.json": HostInfo{Node: "node", KVM: true},
	}
	for name, value := range metadata {
		if err := writeRunFile(dir, name, value); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (m *scenarioMock) serialReply(command string) string {
	switch {
	case command == "help":
		return "hostname date head tail grep wc find hexdump"
	case strings.HasPrefix(command, "hostname "):
		m.hostname = strings.Fields(command)[1]
		m.hostname = strings.TrimSuffix(m.hostname, ";")
		return m.hostname
	case command == "date -u":
		return "2026-10-11T12:00:00Z"
	case strings.HasPrefix(command, "echo alpha "):
		return "alpha\nbeta\ngamma"
	case strings.HasPrefix(command, "head "):
		return "alpha"
	case strings.HasPrefix(command, "tail "):
		return "gamma"
	case strings.HasPrefix(command, "grep "):
		return "beta"
	case strings.HasPrefix(command, "wc "):
		return "3"
	case strings.HasPrefix(command, "find "):
		return "/home/user/pvelab-admin.txt"
	case strings.HasPrefix(command, "hexdump "):
		return "00000000  61 6c 70 68 61 0a |alpha.|"
	case strings.HasPrefix(command, "log "):
		return "[2026-10-11T12:00:00Z] info kernel: event"
	case command == "net status":
		return "state: ready\nlink: up\naddress: 10.0.0.42/24"
	case strings.HasPrefix(command, "remote key add "):
		return m.add(command)
	case command == "config set system.remote.enabled true; remote reload":
		return "CONFIG_SET_OK\nservice=running enabled=true listener=listening settings=configured bind=0.0.0.0:22 user=user fingerprint=" + m.fingerprint + fmt.Sprintf(" generation=%d", m.generation)
	case strings.Contains(command, "remote status"):
		prefix := "GoOS\n"
		if strings.Contains(command, "net status") {
			prefix += m.serialReply("net status") + "\n"
		}
		return prefix + "service=running enabled=true listener=listening settings=configured bind=0.0.0.0:22 user=user fingerprint=" + m.fingerprint + fmt.Sprintf(" generation=%d", m.generation)
	case command == "ps":
		if m.replacePID && len(m.terminal.sizes) > 0 {
			m.pid++
		}
		return fmt.Sprintf("%d native running 5 8 /bin/gfm\n", m.pid)
	case command == "config get system.remote.host-key":
		return "/state/security/ssh/ssh_host_ed25519_key"
	case command == "uname":
		return "GoOS"
	default:
		m.t.Errorf("unexpected serial command %q", command)
		return "unexpected"
	}
}

func (m *scenarioMock) add(command string) string {
	text := strings.Trim(strings.TrimPrefix(command, "remote key add "), "\"")
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		m.t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(key)
	m.keys[fingerprint] = true
	return "authorized keys committed fingerprint=" + fingerprint + " generation=1"
}

func argValue(args []string, option string) string {
	for i, arg := range args {
		if arg == option && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (m *scenarioMock) run(ctx context.Context, name string, args ...string) (commandResult, error) {
	if name == "ssh-keyscan" {
		public := m.public
		if m.mismatch {
			public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testPublic(m.t))))
		}
		return commandResult{Stdout: "10.0.0.42 " + public + "\n"}, nil
	}
	if name != "ssh" {
		return commandResult{}, errors.New("unexpected host tool")
	}
	m.sshCalls++
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=" + filepath.Join(m.s.EvidenceDir, "guest-known-hosts"), "GlobalKnownHostsFile=/dev/null", "IdentityAgent=none"} {
		if !hasArg(args, want) {
			m.t.Errorf("unsafe SSH call missing %s", want)
		}
	}
	known, err := os.ReadFile(filepath.Join(m.s.EvidenceDir, "guest-known-hosts"))
	if err != nil || !strings.Contains(string(known), m.public) {
		m.t.Fatalf("SSH before pin: %q %v", known, err)
	}
	key, err := os.ReadFile(argValue(args, "-i"))
	if err != nil {
		m.t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		m.t.Fatal(err)
	}
	if !m.keys[ssh.FingerprintSHA256(signer.PublicKey())] {
		return commandResult{Status: 255, Stderr: "user@10.0.0.42: Permission denied (publickey)."}, nil
	}
	command := args[len(args)-1]
	if hasArg(args, "-tt") {
		return commandResult{Status: 255, Stderr: "exec request failed on channel 0"}, nil
	}
	switch {
	case command == "uname":
		return commandResult{Stdout: "GoOS\n"}, nil
	case command == "net status":
		return commandResult{Stdout: m.serialReply(command)}, nil
	case command == "log -n 5":
		return commandResult{Stdout: m.serialReply(command)}, nil
	case strings.Contains(command, "pvelab-does-not-exist"):
		return commandResult{Status: 1, Stderr: "cat: pvelab-does-not-exist: no such file or directory"}, nil
	case strings.HasSuffix(command, " &"):
		return commandResult{Status: 2, Stderr: "background forbidden"}, nil
	case command == "remote reload":
		m.generation++
		if m.terminal.following {
			generation := m.generation
			if m.dropFollowEvents > 0 {
				m.dropFollowEvents--
				generation = 1
			}
			m.terminal.pending = append(m.terminal.pending, []byte(fmt.Sprintf("\n[2026-10-11T12:00:01Z] INFO  remoted[7]: remote configuration committed generation=%d\n", generation))...)
		}
		return commandResult{Stdout: m.serialReply("remote status")}, nil
	case strings.HasPrefix(command, "remote key add "):
		return commandResult{Stdout: m.add(command)}, nil
	case strings.HasPrefix(command, "remote key remove "):
		fingerprint := strings.TrimPrefix(command, "remote key remove ")
		delete(m.keys, fingerprint)
		return commandResult{Stdout: "authorized keys committed fingerprint=" + fingerprint}, nil
	case strings.HasPrefix(command, "remote keys"):
		var out strings.Builder
		for fingerprint := range m.keys {
			out.WriteString(fingerprint + "\n")
		}
		if strings.Contains(command, "uname") {
			out.WriteString("GoOS\n")
		}
		return commandResult{Stdout: out.String()}, nil
	case strings.HasPrefix(command, "cat /state/"):
		if m.privateLeak != "" {
			return commandResult{Stdout: m.privateLeak}, nil
		}
		return commandResult{Status: 1, Stderr: "permission denied"}, nil
	case command == "reboot":
		m.events = append(m.events, "guest-reboot")
		serial := m.s.Guest.con.(*scriptConsole)
		if !m.noReboot {
			serial.pending = append(serial.pending, []byte(normalBoot)...)
		}
		return commandResult{}, nil
	case command == "uname; net status":
		return commandResult{Stdout: "GoOS\n" + m.serialReply("net status")}, nil
	case command == "hostname":
		return commandResult{Stdout: m.hostname + "\n"}, nil
	case command == "poweroff":
		m.events = append(m.events, "guest-poweroff")
		m.power = "stopped"
		serial := m.s.Guest.con.(*scriptConsole)
		serial.pending = append(serial.pending, []byte("\nPowering off...\n")...)
		return commandResult{}, nil
	default:
		return commandResult{}, fmt.Errorf("unexpected SSH command %q", command)
	}
}

func TestRemoteAdminScenarioMock(t *testing.T) {
	m := newScenarioMock(t)
	result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err != nil || result != ResultPass {
		t.Fatalf("result %s: %v\n%+v", result, err, m.s.Report.Remote)
	}
	r := m.s.Report.Remote
	if len(r.Assertions) != len(remoteAdminChecks) || r.ResizedPID != 42 || r.PoweroffState != "stopped" || r.FinalPowerState != "running" {
		t.Fatalf("report %+v", r)
	}
	for _, assertion := range r.Assertions {
		if assertion.Status != "pass" {
			t.Errorf("assertion %+v", assertion)
		}
	}
	if !m.terminal.exited || len(m.terminal.sizes) != 3 || len(m.keys) != 1 {
		t.Fatalf("terminal or key lifecycle incomplete")
	}
	events := strings.Join(m.events, ",")
	if !strings.Contains(events, "guest-reboot,serial-reconnect") || !strings.Contains(events, "guest-poweroff,state:stopped,start-retained,serial-reconnect,state:running") {
		t.Fatalf("lifecycle order %s", events)
	}
	if !fileExists(filepath.Join(m.s.EvidenceDir, "remote-admin.json")) {
		t.Fatal("missing report evidence")
	}
}

func TestRemoteAdminMismatchBeforeAuthentication(t *testing.T) {
	m := newScenarioMock(t)
	m.mismatch = true
	result, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || result == ResultPass || m.sshCalls != 0 {
		t.Fatalf("authenticated before trusted pin: %s %v calls=%d", result, err, m.sshCalls)
	}
	for _, assertion := range m.s.Report.Remote.Assertions {
		if assertion.Name == "host-key-pin" && assertion.Status != "fail" {
			t.Fatalf("pin assertion %+v", assertion)
		}
		if assertion.Name == "exec-uname" && assertion.Status != "not-run" {
			t.Fatalf("exec falsely passed %+v", assertion)
		}
	}
}

func TestRemoteAdminResizeDoesNotReplaceProcess(t *testing.T) {
	m := newScenarioMock(t)
	m.replacePID = true
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement not detected: %v", err)
	}
}

func TestRemoteAdminMissingBridgeBlocked(t *testing.T) {
	m := newScenarioMock(t)
	m.s.Report.VM.NetBridge = ""
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err == nil || classOf(err) != ClassInfra || m.s.Report.Remote.Assertions[0].Status != "blocked" || m.sshCalls != 0 {
		t.Fatalf("missing bridge not blocked %v", err)
	}
}

func TestRemoteAdminNonKeptDoesNotRestart(t *testing.T) {
	m := newScenarioMock(t)
	m.s.KeepRunning = false
	_, err := (remoteAdminScenario{}).Run(context.Background(), m.s)
	if err != nil {
		t.Fatal(err)
	}
	if m.s.Report.Remote.FinalPowerState != "stopped" || strings.Contains(strings.Join(m.events, ","), "start-retained") {
		t.Fatalf("unexpected restart %+v", m.events)
	}
}
