package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const remoteAdminTimeout = 12 * time.Minute

var remoteAdminChecks = []string{
	"serial-help", "serial-hostname", "serial-date", "serial-fixture",
	"serial-head", "serial-tail", "serial-grep", "serial-wc", "serial-find", "serial-hexdump", "serial-log",
	"guest-address", "serial-primary-key", "serial-remote-ready", "host-key-pin",
	"exec-uname", "exec-net-status", "exec-log", "exec-stderr-status", "exec-background-rejected", "exec-pty-rejected",
	"log-follow", "log-follow-cancel", "gfm-live-resize", "interactive-exit",
	"second-key-add", "second-key-list", "second-key-auth", "second-key-remove", "second-key-refused", "primary-key-preserved",
	"host-private-key-denied", "reboot-clean-close", "reboot-boot-normal", "reboot-serial", "reboot-host-key-pin", "reboot-reconnect", "hostname-persisted",
	"poweroff-clean-close", "poweroff-stopped", "final-power-state", "evidence-files",
}

type RemoteAssertion struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // not-run, pass, fail, or blocked
	Observed string `json:"observed,omitempty"`
}

type RemoteAdminReport struct {
	Assertions           []RemoteAssertion `json:"assertions"`
	SSH                  []RemoteExec      `json:"ssh"`
	Address              string            `json:"address,omitempty"`
	InitialAddress       string            `json:"initial_address,omitempty"`
	Port                 int               `json:"port,omitempty"`
	HostFingerprint      string            `json:"host_fingerprint,omitempty"`
	PrimaryFingerprint   string            `json:"primary_fingerprint,omitempty"`
	SecondaryFingerprint string            `json:"secondary_fingerprint,omitempty"`
	PoweroffState        string            `json:"poweroff_state,omitempty"`
	FinalPowerState      string            `json:"final_power_state,omitempty"`
	ResizedPID           int               `json:"resized_pid,omitempty"`
	EvidenceFiles        []string          `json:"evidence_files,omitempty"`
}

type RemoteExec struct {
	Check   string `json:"check"`
	Command string `json:"command"`
	commandResult
}

type remoteAdminScenario struct{}

func (remoteAdminScenario) Name() string { return "remote-admin" }

func (remoteAdminScenario) Run(ctx context.Context, s *Session) (result Result, runErr error) {
	ctx, cancel := context.WithTimeout(ctx, remoteAdminTimeout)
	defer cancel()
	r := &RemoteAdminReport{}
	for _, name := range remoteAdminChecks {
		r.Assertions = append(r.Assertions, RemoteAssertion{Name: name, Status: "not-run"})
	}
	s.Report.Remote = r
	if s.Report.VM.NetBridge == "" || s.ReconnectSerial == nil || s.StartRetained == nil || s.PowerState == nil || s.EvidenceDir == "" {
		r.Assertions[0].Status = "blocked"
		r.Assertions[0].Observed = "requires a bridged NIC, serial reconnect/start callbacks, and evidence directory"
		return "", failf(ClassInfra, "remote-admin", "%s", r.Assertions[0].Observed)
	}
	if s.HostRunner == nil {
		s.HostRunner = runHostCommand
	}
	if s.OpenTerminal == nil {
		s.OpenTerminal = openGuestTerminal
	}
	defer func() {
		if err := writeRunFile(s.EvidenceDir, "remote-admin.json", r); err != nil {
			runErr = errors.Join(runErr, fail(ClassInfra, "remote-evidence", err))
			result = ""
		}
	}()
	keysDir, err := os.MkdirTemp("", "pvelab-client-")
	if err != nil {
		return "", fail(ClassInfra, "client-keys", err)
	}
	defer func() {
		if err := os.RemoveAll(keysDir); err != nil {
			runErr = errors.Join(runErr, fail(ClassInfra, "client-key-cleanup", err))
			result = ""
		}
	}()
	primary, primaryLine, primaryFP, err := makeClientKey(keysDir, "primary")
	if err != nil {
		return "", fail(ClassInfra, "client-keys", err)
	}
	secondary, secondaryLine, secondaryFP, err := makeClientKey(keysDir, "secondary")
	if err != nil {
		return "", fail(ClassInfra, "client-keys", err)
	}
	r.PrimaryFingerprint, r.SecondaryFingerprint = primaryFP, secondaryFP
	check := func(name string, fn func() (string, error)) error {
		return observeRemoteCheck(s.Guest, r, name, fn)
	}
	serial := func(name, command string, valid func(string) bool) error {
		return check(name, func() (string, error) {
			out, err := s.Guest.Run(ctx, command, commandTimeout)
			if err == nil {
				err = s.Guest.Clean(name, out)
			}
			if err == nil && !valid(out) {
				err = errors.New("serial output did not satisfy assertion")
			}
			return command + "\n" + out, err
		})
	}
	contains := func(want string) func(string) bool {
		return func(out string) bool { return strings.Contains(out, want) }
	}
	line := func(want string) func(string) bool { return func(out string) bool { return hasLine(out, want) } }
	exact := func(want string) func(string) bool {
		return func(out string) bool { return strings.TrimSpace(strings.ReplaceAll(out, "\r", "")) == want }
	}
	hostname := "pve-" + strings.ReplaceAll(s.Report.RunID, "_", "-")
	if len(hostname) > 63 {
		hostname = hostname[:63]
	}
	fixture := "/home/user/pvelab-admin.txt"
	serialSteps := []struct {
		name, command string
		valid         func(string) bool
	}{
		{"serial-help", "help", contains("hostname")},
		{"serial-hostname", "hostname " + hostname + "; hostname", line(hostname)},
		{"serial-date", "date -u", validUTCDate},
		{"serial-fixture", "echo alpha > " + fixture + "; echo beta >> " + fixture + "; echo gamma >> " + fixture + "; cat " + fixture, func(out string) bool { return hasLine(out, "alpha") && hasLine(out, "beta") && hasLine(out, "gamma") }},
		{"serial-head", "head -n 1 " + fixture, exact("alpha")},
		{"serial-tail", "tail -n 1 " + fixture, exact("gamma")},
		{"serial-grep", "grep beta " + fixture, exact("beta")},
		{"serial-wc", "wc -l " + fixture, exact("3")},
		{"serial-find", "find /home/user -name pvelab-admin.txt", contains(fixture)},
		{"serial-hexdump", "hexdump " + fixture, func(out string) bool {
			return strings.Contains(out, "00000000") && strings.Contains(out, "61 6c 70 68 61")
		}},
		{"serial-log", "log -n 5", validLogSnapshot},
	}
	for _, step := range serialSteps {
		if err := serial(step.name, step.command, step.valid); err != nil {
			return "", err
		}
	}
	var address string
	if err := check("guest-address", func() (string, error) {
		var out string
		leaseCtx, stop := context.WithTimeout(ctx, networkReadyTimeout)
		defer stop()
		for {
			var err error
			out, err = s.Guest.Run(leaseCtx, "net status", commandTimeout)
			if err != nil {
				return out, err
			}
			if address, err = reachableGuestAddress(out); err == nil {
				return out, nil
			}
			if err := pause(leaseCtx, time.Second); err != nil {
				return out, fail(ClassInfra, "guest-address", err)
			}
		}
	}); err != nil {
		return "", err
	}
	if err := serial("serial-primary-key", "remote key add \""+primaryLine+"\"", func(out string) bool { return strings.Contains(out, primaryFP) }); err != nil {
		return "", err
	}
	var status map[string]string
	if err := check("serial-remote-ready", func() (string, error) {
		out, err := s.Guest.Run(ctx, "config set system.remote.enabled true; remote reload", commandTimeout)
		if err != nil {
			return out, err
		}
		if err := s.Guest.Clean("serial-remote-ready", out); err != nil {
			return out, err
		}
		status, err = remoteStatusFields(out)
		return out, err
	}); err != nil {
		return "", err
	}
	_, portText, err := net.SplitHostPort(status["bind"])
	if err != nil {
		return "", fail(ClassScenario, "serial-remote-ready", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", failf(ClassScenario, "serial-remote-ready", "invalid SSH port %q", portText)
	}
	client := guestSSH{Host: address, Port: port, User: status["user"], Key: primary, KnownHosts: filepath.Join(s.EvidenceDir, "guest-known-hosts"), run: s.HostRunner}
	r.Address, r.InitialAddress, r.Port, r.HostFingerprint = address, address, port, status["fingerprint"]
	pin := func(name string, preserve bool) error {
		return check(name, func() (string, error) {
			scanCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			defer stop()
			scanned, err := s.HostRunner(scanCtx, "ssh-keyscan", "-T", "5", "-p", strconv.Itoa(port), "-t", "ed25519", address)
			if err != nil {
				return scanned.Stderr, fail(ClassInfra, name, err)
			}
			if scanned.Status != 0 {
				return scanned.Stderr, failf(ClassInfra, name, "ssh-keyscan exited %d", scanned.Status)
			}
			known, err := pinHostKey(scanned.Stdout, status["fingerprint"], address, port)
			if err != nil {
				return scanned.Stdout, err
			}
			if preserve {
				old, err := os.ReadFile(client.KnownHosts)
				if err != nil {
					return "", fail(ClassInfra, name, err)
				}
				if !bytes.Equal(old, known) {
					_, oldKey, _ := strings.Cut(string(old), " ")
					_, newKey, _ := strings.Cut(string(known), " ")
					if oldKey != newKey {
						return string(known), errors.New("exact pinned host key changed after reboot")
					}
					// A renewed lease may change the target address, never its identity.
					if err := os.WriteFile(client.KnownHosts, known, 0o600); err != nil {
						return "", fail(ClassInfra, name, err)
					}
				}
			} else if err := os.WriteFile(client.KnownHosts, known, 0o600); err != nil {
				return "", fail(ClassInfra, name, err)
			}
			return string(known) + status["fingerprint"], nil
		})
	}
	if err := pin("host-key-pin", false); err != nil {
		return "", err
	}
	execute := func(name string, sshClient guestSSH, command string, valid func(commandResult) bool) error {
		return check(name, func() (string, error) {
			out, err := sshClient.exec(ctx, command)
			accepted := err == nil && valid(out)
			if name == "host-private-key-denied" {
				// If protection regresses, never persist the disclosed key as evidence.
				out.Stdout = fmt.Sprintf("<redacted: %d stdout bytes>", len(out.Stdout))
				out.Stderr = fmt.Sprintf("<redacted: %d stderr bytes>", len(out.Stderr))
			}
			r.SSH = append(r.SSH, RemoteExec{Check: name, Command: command, commandResult: out})
			if err != nil {
				return string(jsonIndent(out)), fail(ClassInfra, name, err)
			}
			if !accepted {
				return string(jsonIndent(out)), errors.New("SSH stdout/stderr/status did not satisfy assertion")
			}
			return string(jsonIndent(out)), nil
		})
	}
	okOutput := func(valid func(string) bool) func(commandResult) bool {
		return func(out commandResult) bool {
			return out.Status == 0 && strings.TrimSpace(out.Stderr) == "" && valid(out.Stdout)
		}
	}
	for _, step := range []struct {
		name, command string
		valid         func(commandResult) bool
	}{
		{"exec-uname", "uname", okOutput(contains("GoOS"))},
		{"exec-net-status", "net status", okOutput(func(out string) bool { ip, err := reachableGuestAddress(out); return err == nil && ip == address })},
		{"exec-log", "log -n 5", okOutput(validLogSnapshot)},
		{"exec-stderr-status", "cat /home/user/pvelab-does-not-exist", func(out commandResult) bool {
			return out.Status > 0 && out.Status < 255 && strings.Contains(out.Stderr, "pvelab-does-not-exist") && strings.TrimSpace(out.Stdout) == ""
		}},
		{"exec-background-rejected", "echo PVELAB_BACKGROUND_FORBIDDEN &", func(out commandResult) bool {
			return out.Status > 0 && out.Status < 255 && out.Stderr != "" && !strings.Contains(out.Stdout, "PVELAB_BACKGROUND_FORBIDDEN")
		}},
	} {
		if err := execute(step.name, client, step.command, step.valid); err != nil {
			return "", err
		}
	}
	if err := check("exec-pty-rejected", func() (string, error) {
		requestCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		runner := s.HostPTYRunner
		if runner == nil {
			runner = runHostPTYCommand
		}
		out, err := runner(requestCtx, "ssh", client.args(true, "uname")...)
		r.SSH = append(r.SSH, RemoteExec{Check: "exec-pty-rejected", Command: "PTY + exec uname", commandResult: out})
		if err != nil {
			return string(jsonIndent(out)), fail(ClassInfra, "exec-pty-rejected", err)
		}
		if out.Status == 0 || !strings.Contains(out.Stderr, "exec request failed") {
			return string(jsonIndent(out)), errors.New("PTY + exec was not explicitly rejected")
		}
		return string(jsonIndent(out)), nil
	}); err != nil {
		return "", err
	}
	terminal, err := s.OpenTerminal(ctx, client, filepath.Join(s.EvidenceDir, "guest-terminal.log"))
	if err != nil {
		return "", fail(ClassInfra, "interactive-open", err)
	}
	defer func() {
		if err := terminal.Close(); err != nil {
			runErr = errors.Join(runErr, fail(ClassInfra, "interactive-evidence", err))
			result = ""
		}
	}()
	if err := remoteWait(ctx, terminal, shellPrompt); err != nil {
		return "", fail(ClassScenario, "interactive-prompt", err)
	}
	if err := check("log-follow", func() (string, error) {
		terminal.Drain()
		if err := terminal.WriteLine("log -f -n 0"); err != nil {
			return "", err
		}
		if err := remoteWait(ctx, terminal, "log -f -n 0"); err != nil {
			return "", err
		}
		terminal.Drain()
		generation, _ := strconv.ParseUint(status["generation"], 10, 64)
		var observed strings.Builder
		// Echo can precede cursor initialization. Bounded new-generation events
		// avoid that race without accepting a delayed unrelated backlog record.
		for attempt := 0; attempt < 3; attempt++ {
			event, err := client.exec(ctx, "remote reload")
			r.SSH = append(r.SSH, RemoteExec{Check: "log-follow-event", Command: "remote reload", commandResult: event})
			if err != nil {
				return string(jsonIndent(event)), fail(ClassInfra, "log-follow", err)
			}
			fields, parseErr := remoteStatusFields(event.Stdout)
			next, numberErr := strconv.ParseUint(fields["generation"], 10, 64)
			if event.Status != 0 || parseErr != nil || numberErr != nil || next <= generation {
				return string(jsonIndent(event)), errors.New("second SSH session did not commit a fresh remote generation")
			}
			generation = next
			matched, out, err := waitFollowGeneration(ctx, terminal, generation)
			observed.WriteString(out)
			if matched {
				return observed.String(), nil
			}
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				return observed.String(), err
			}
			if ctx.Err() != nil {
				return observed.String(), ctx.Err()
			}
		}
		return observed.String(), errors.New("follow did not observe any of three new generation-tagged events")
	}); err != nil {
		return "", err
	}
	if err := check("log-follow-cancel", func() (string, error) {
		if err := terminal.Write([]byte{3}); err != nil {
			return "", err
		}
		if err := remoteWait(ctx, terminal, "\n"+shellPrompt); err != nil {
			return "", err
		}
		return "Ctrl-C returned to the same interactive shell", nil
	}); err != nil {
		return "", err
	}
	if err := check("gfm-live-resize", func() (string, error) { return observeLiveResize(ctx, s.Guest, terminal, r) }); err != nil {
		return "", err
	}
	if err := check("interactive-exit", func() (string, error) {
		if err := terminal.WriteLine("exit"); err != nil {
			return "", err
		}
		closeCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		out, err := terminal.Wait(closeCtx)
		return string(jsonIndent(out)), err
	}); err != nil {
		return "", err
	}
	if err := terminal.Close(); err != nil {
		return "", fail(ClassInfra, "interactive-close", err)
	}
	secondClient := client
	secondClient.Key = secondary
	for _, step := range []struct {
		name    string
		client  guestSSH
		command string
		valid   func(commandResult) bool
	}{
		{"second-key-add", client, "remote key add \"" + secondaryLine + "\"", okOutput(contains(secondaryFP))},
		{"second-key-list", client, "remote keys", okOutput(func(out string) bool { return strings.Contains(out, primaryFP) && strings.Contains(out, secondaryFP) })},
		{"second-key-auth", secondClient, "uname", okOutput(contains("GoOS"))},
		{"second-key-remove", client, "remote key remove " + secondaryFP, okOutput(contains(secondaryFP))},
		{"second-key-refused", secondClient, "uname", func(out commandResult) bool {
			return out.Status == 255 && strings.Contains(out.Stderr, "Permission denied (publickey)") && !strings.Contains(out.Stdout, "GoOS")
		}},
		{"primary-key-preserved", client, "remote keys; uname", okOutput(func(out string) bool {
			return strings.Contains(out, primaryFP) && !strings.Contains(out, secondaryFP) && strings.Contains(out, "GoOS")
		})},
	} {
		if err := execute(step.name, step.client, step.command, step.valid); err != nil {
			return "", err
		}
	}
	privatePath, err := s.Guest.Run(ctx, "config get system.remote.host-key", commandTimeout)
	if err != nil {
		return "", err
	}
	privatePath = strings.TrimSpace(strings.Split(privatePath, "\n")[0])
	if !strings.HasPrefix(privatePath, "/state/") || strings.ContainsAny(privatePath, "\r\n\x00") {
		return "", failf(ClassScenario, "host-private-key-denied", "invalid configured private path %q", privatePath)
	}
	if err := execute("host-private-key-denied", client, "cat "+shellQuote(privatePath), func(out commandResult) bool {
		return out.Status > 0 && out.Status < 255 && strings.Contains(strings.ToLower(out.Stderr), "permission denied") && strings.TrimSpace(out.Stdout) == ""
	}); err != nil {
		return "", err
	}
	if err := execute("reboot-clean-close", client, "reboot", okOutput(func(string) bool { return true })); err != nil {
		return "", err
	}
	if err := check("reboot-boot-normal", func() (string, error) {
		err := s.Guest.WaitBoot(ctx, promptTimeout)
		return "new normal-mode boot marker and shell prompt on retained serial transport", err
	}); err != nil {
		return "", err
	}
	if err := check("reboot-serial", func() (string, error) {
		if err := s.ReconnectSerial(ctx); err != nil {
			return "", fail(ClassInfra, "reboot-serial", err)
		}
		readyCtx, stop := context.WithTimeout(ctx, networkReadyTimeout)
		defer stop()
		for {
			out, err := s.Guest.Run(readyCtx, "uname; net status; remote status", commandTimeout)
			if err != nil {
				return out, err
			}
			fields, statusErr := remoteStatusFields(out)
			renewed, addressErr := reachableGuestAddress(out)
			if statusErr == nil && addressErr == nil {
				if !strings.Contains(out, "GoOS") || fields["fingerprint"] != r.HostFingerprint || fields["user"] != client.User || fields["bind"] != status["bind"] {
					return out, errors.New("reboot did not retain guest identity/configuration")
				}
				status = fields
				address, client.Host, r.Address = renewed, renewed, renewed
				return out, nil
			}
			if err := pause(readyCtx, time.Second); err != nil {
				return out, fail(ClassInfra, "reboot-network", err)
			}
		}
	}); err != nil {
		return "", err
	}
	if err := pin("reboot-host-key-pin", true); err != nil {
		return "", err
	}
	if err := execute("reboot-reconnect", client, "uname; net status", okOutput(func(out string) bool {
		ip, err := reachableGuestAddress(out)
		return err == nil && ip == address && strings.Contains(out, "GoOS")
	})); err != nil {
		return "", err
	}
	if err := execute("hostname-persisted", client, "hostname", okOutput(line(hostname))); err != nil {
		return "", err
	}
	if err := execute("poweroff-clean-close", client, "poweroff", okOutput(func(string) bool { return true })); err != nil {
		return "", err
	}
	if err := check("poweroff-stopped", func() (string, error) {
		offCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		_, serialErr := s.Guest.con.ReadUntil(offCtx, []byte("Powering off"))
		stop()
		if serialErr != nil {
			return "missing serial poweroff marker", serialErr
		}
		state, err := waitPowerState(ctx, s.PowerState, "stopped")
		r.PoweroffState = state
		return state, err
	}); err != nil {
		return "", err
	}
	if err := check("final-power-state", func() (string, error) {
		want := "stopped"
		if s.KeepRunning {
			if err := s.StartRetained(ctx); err != nil {
				return "", fail(ClassInfra, "retained-start", err)
			}
			if err := s.ReconnectSerial(ctx); err != nil {
				return "", fail(ClassInfra, "retained-serial", err)
			}
			out, err := s.Guest.Run(ctx, "uname", commandTimeout)
			if err != nil || !strings.Contains(out, "GoOS") {
				return out, errors.Join(err, errors.New("retained VM shell not ready"))
			}
			want = "running"
		}
		state, err := waitPowerState(ctx, s.PowerState, want)
		r.FinalPowerState = state
		return state, err
	}); err != nil {
		return "", err
	}
	if err := check("evidence-files", func() (string, error) {
		r.EvidenceFiles = []string{"serial.log", "state.json", "artifacts.json", "vm-config.json", "host-info.json", "guest-known-hosts", "guest-terminal.log", "remote-admin.json"}
		if err := writeRunFile(s.EvidenceDir, "remote-admin.json", r); err != nil {
			return "", fail(ClassInfra, "evidence-files", err)
		}
		if err := validateRemoteEvidence(s.EvidenceDir, s.Report, r.EvidenceFiles); err != nil {
			return "", fail(ClassInfra, "evidence-files", err)
		}
		return strings.Join(r.EvidenceFiles, ", "), nil
	}); err != nil {
		return "", err
	}
	for _, assertion := range r.Assertions {
		if assertion.Status != "pass" {
			return "", failf(ClassScenario, "remote-admin", "assertion %s is %s", assertion.Name, assertion.Status)
		}
	}
	return ResultPass, nil
}

func validateRemoteEvidence(dir string, report *Report, names []string) error {
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("missing, empty, or nonregular evidence %s", name)
		}
		if strings.HasSuffix(name, ".json") {
			if info.Size() > guestOutputLimit {
				return fmt.Errorf("oversized JSON evidence %s", name)
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !json.Valid(data) {
				return fmt.Errorf("invalid JSON evidence %s", name)
			}
		}
	}
	if report.Git.Dirty || !validHexDigest(report.Git.SHA, 40) || !validHexDigest(report.Artifacts.ISOSHA256, 64) || !validHexDigest(report.Artifacts.DiskSHA256, 64) {
		return errors.New("evidence requires clean candidate SHA and artifact hashes")
	}
	var state State
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	sourceSHA := report.Git.SHA
	if resume := report.Resume; resume != nil {
		if resume.SourceRunID != report.RunID || resume.SourceGitSHA == "" || !validSHA256(resume.SourceEvidenceSHA256) || resume.CandidateGitSHA != report.Git.SHA ||
			resume.SourceISOSHA256 != report.Artifacts.ISOSHA256 || resume.SourceDiskSHA256 != report.Artifacts.DiskSHA256 {
			return errors.New("resume provenance does not match candidate or source artifacts")
		}
		sourceSHA = resume.SourceGitSHA
	}
	if state.RunID != report.RunID || state.GitSHA != sourceSHA || state.VMID != report.Proxmox.VMID || state.Node != report.Proxmox.Node {
		return errors.New("state evidence does not match source run, VM, or node")
	}
	var artifacts Artifacts
	data, err = os.ReadFile(filepath.Join(dir, "artifacts.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return err
	}
	if artifacts.ISO.SHA256 != report.Artifacts.ISOSHA256 || artifacts.Disk.SHA256 != report.Artifacts.DiskSHA256 || artifacts.ISO.Size <= 0 || artifacts.Disk.Size <= 0 {
		return errors.New("artifact evidence does not match uploaded inputs")
	}
	return nil
}

func validHexDigest(text string, length int) bool {
	if len(text) != length {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

func observeRemoteCheck(g *Guest, r *RemoteAdminReport, name string, fn func() (string, error)) error {
	for i := range r.Assertions {
		a := &r.Assertions[i]
		if a.Name != name {
			continue
		}
		observed, err := fn()
		a.Observed = observed
		if err != nil {
			a.Status = "fail"
			var stage *stageError
			if errors.As(err, &stage) && stage.class == ClassInfra {
				a.Status = "blocked"
			}
			return fail(ClassScenario, name, err)
		}
		a.Status = "pass"
		g.pass(name)
		return nil
	}
	return failf(ClassConfig, name, "unregistered remote assertion")
}

func validUTCDate(out string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if _, err := time.Parse("2006-01-02T15:04:05Z", strings.TrimSpace(line)); err == nil {
			return true
		}
	}
	return false
}

var logRecord = regexp.MustCompile(`(?m)^\[20[0-9]{2}-[0-9]{2}-[0-9]{2}T[^\]\r\n]+Z\]`)

func validLogSnapshot(out string) bool {
	count := len(logRecord.FindAllStringIndex(strings.ReplaceAll(out, "\r", ""), -1))
	return count > 0 && count <= 5
}

func reachableGuestAddress(out string) (string, error) {
	fields := statusFields(out)
	if fields["state"] != "ready" || fields["link"] != "up" {
		return "", errors.New("guest NIC is not ready")
	}
	text := strings.Split(fields["address"], "/")[0]
	ip, err := netip.ParseAddr(text)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return "", errors.New("net status has no reachable IPv4 address")
	}
	return ip.String(), nil
}

func remoteStatusFields(out string) (map[string]string, error) {
	var found map[string]string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "service=running ") {
			continue
		}
		if found != nil {
			return nil, errors.New("multiple remote status lines")
		}
		found = map[string]string{}
		for _, word := range strings.Fields(line) {
			if key, value, ok := strings.Cut(word, "="); ok {
				found[key] = value
			}
		}
	}
	if found["listener"] != "listening" || found["settings"] != "configured" || found["enabled"] != "true" || !validRemoteUser(found["user"]) || !strings.HasPrefix(found["fingerprint"], "SHA256:") {
		return nil, errors.New("remote listener not configured with trusted host identity")
	}
	return found, nil
}

func validRemoteUser(user string) bool {
	if len(user) < 1 || len(user) > 64 {
		return false
	}
	for _, c := range user {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func waitFollowGeneration(ctx context.Context, terminal Console, generation uint64) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pattern := regexp.MustCompile(`^\[20[0-9]{2}-[0-9]{2}-[0-9]{2}T[^\]\r\n]+Z\] INFO +remoted\[[0-9]+\]: remote configuration committed generation=` + strconv.FormatUint(generation, 10) + `$`)
	var observed strings.Builder
	for {
		out, err := terminal.ReadUntil(ctx, []byte("\n"))
		observed.Write(out)
		if err != nil {
			return false, observed.String(), err
		}
		if pattern.MatchString(strings.TrimRight(string(out), "\r\n")) {
			return true, observed.String(), nil
		}
	}
}

func remoteWait(ctx context.Context, terminal Console, marker string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := terminal.ReadUntil(ctx, []byte(marker))
	return err
}

func pause(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(duration):
		return nil
	}
}

func waitPowerState(ctx context.Context, state func(context.Context) (string, error), want string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var observed string
	for {
		var err error
		observed, err = state(ctx)
		if err != nil {
			return observed, fail(ClassInfra, "power-state", err)
		}
		if observed == want {
			return observed, nil
		}
		if err := pause(ctx, time.Second); err != nil {
			return observed, err
		}
	}
}

func observeLiveResize(ctx context.Context, g *Guest, terminal remoteTerminal, r *RemoteAdminReport) (string, error) {
	terminal.Drain()
	if err := terminal.WriteLine("gfm"); err != nil {
		return "", err
	}
	var pid int
	var evidence []string
	for i, size := range [][2]int{{80, 24}, {120, 40}, {100, 30}, {132, 44}} {
		if i > 0 {
			terminal.Drain()
			if err := terminal.Resize(size[0], size[1]); err != nil {
				return strings.Join(evidence, "\n"), err
			}
		}
		marker := fmt.Sprintf("\x1b[%d;1H", size[1])
		if err := remoteWait(ctx, terminal, marker); err != nil {
			return strings.Join(evidence, "\n"), err
		}
		footerCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		footer, err := terminal.ReadUntil(footerCtx, []byte("Quit"))
		stop()
		if err != nil {
			return strings.Join(evidence, "\n"), err
		}
		visible := ansiControl.ReplaceAllString(string(footer), "")
		if len(visible) != gfmQuitColumn(size[0]) || !strings.Contains(visible, "F10") {
			return string(footer), fmt.Errorf("gfm footer Quit ended at column %d, want %d for %d columns", len(visible), gfmQuitColumn(size[0]), size[0])
		}
		out, err := g.Run(ctx, "ps", commandTimeout)
		if err != nil {
			return out, err
		}
		observed, err := foregroundPID(out, "gfm")
		if err != nil {
			return out, err
		}
		if i > 0 && observed != pid {
			return out, errors.New("gfm process replaced during live resize")
		}
		pid = observed
		evidence = append(evidence, fmt.Sprintf("%dx%d footer row %d, gfm PID %d\n%s", size[0], size[1], size[1], pid, out))
	}
	r.ResizedPID = pid
	if err := terminal.Write([]byte("\x1b[21~")); err != nil {
		return strings.Join(evidence, "\n"), err
	}
	if err := remoteWait(ctx, terminal, "\n"+shellPrompt); err != nil {
		return strings.Join(evidence, "\n"), err
	}
	return strings.Join(evidence, "\n"), nil
}

var ansiControl = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

// The existing nine-button gfm footer shares spare width evenly. Observing
// the last label's column verifies width as well as the footer's row.
func gfmQuitColumn(columns int) int {
	const naturalWidth = 78
	spare := max((columns-1-naturalWidth)/9, 0)
	return 77 + 8*spare
}

func foregroundPID(out, name string) (int, error) {
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 || fields[1] != "native" || filepath.Base(fields[5]) != name {
			continue
		}
		value, err := strconv.Atoi(fields[0])
		if err != nil || value < 1 || pid != 0 {
			return 0, errors.New("invalid or multiple foreground processes")
		}
		pid = value
	}
	if pid == 0 {
		return 0, errors.New("foreground process absent from serial ps")
	}
	return pid, nil
}
