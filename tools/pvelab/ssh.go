package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
)

// stagingRoot is the only remote directory pvelab ever removes anything
// under; each run owns stagingRoot/<run-id>.
const stagingRoot = "/var/tmp/goos-pvelab"

// SSH runs commands on the Proxmox node through the system ssh binary, so
// the user's agent, keys, and known_hosts apply unchanged. Host keys must
// already be known: an unknown or changed key is an error.
type SSH struct {
	Host string
	Port int
	User string
	Key  string
	Sudo bool
	logf func(string, ...any)
}

func NewSSH(cfg *Config, logf func(string, ...any)) SSH {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return SSH{Host: cfg.SSHHost, Port: cfg.SSHPort, User: cfg.SSHUser, Key: cfg.SSHKey, Sudo: cfg.Sudo(), logf: logf}
}

func (s SSH) args(remote string) []string {
	args := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-p", strconv.Itoa(s.Port),
		"-l", s.User,
	}
	if s.Key != "" {
		args = append(args, "-i", s.Key)
	}
	return append(args, s.Host, remote)
}

// shellQuote quotes one word for a POSIX shell. Every dynamic value in a
// remote command goes through it.
func shellQuote(word string) string {
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@+%", r))
	}) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func shellJoin(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// Root prefixes a command that needs root on the node.
func (s SSH) Root(argv ...string) []string {
	if s.Sudo {
		return append([]string{"sudo", "-n"}, argv...)
	}
	return argv
}

func (s SSH) Command(ctx context.Context, remote string) *exec.Cmd {
	s.logf("ssh %s", remote)
	cmd := exec.CommandContext(ctx, "ssh", s.args(remote)...)
	// Its own process group: Ctrl-C reaches pvelab, which cancels the
	// context and cleans up, not ssh directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// Run executes argv on the node and returns its standard output.
func (s SSH) Run(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := s.Command(ctx, shellJoin(argv...))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", commandName(argv), err, lastLines(stderr.String()+stdout.String(), 8))
	}
	return stdout.Bytes(), nil
}

// commandName is the program argv runs, past a sudo prefix.
func commandName(argv []string) string {
	for i, a := range argv {
		if a == "sudo" || a == "-n" {
			continue
		}
		if i+1 < len(argv) && (a == "qm" || a == "pvesm") {
			return a + " " + argv[i+1]
		}
		return a
	}
	return "command"
}

// CopyTo streams local into the remote file through gzip. Test disks are
// mostly zeros, so this moves a few MiB instead of the whole image.
func (s SSH) CopyTo(ctx context.Context, local, remote string, progress func(int64)) error {
	file, err := os.Open(local)
	if err != nil {
		return err
	}
	defer file.Close()
	cmd := s.Command(ctx, "gzip -dc > "+shellQuote(remote))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(stdin, gzip.BestSpeed)
	_, copyErr := io.Copy(zw, &countingReader{r: file, report: progress})
	if copyErr == nil {
		copyErr = zw.Close()
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if copyErr != nil {
		return fmt.Errorf("sending %s: %w: %s", local, copyErr, lastLines(stderr.String(), 4))
	}
	if waitErr != nil {
		return fmt.Errorf("writing %s: %w: %s", remote, waitErr, lastLines(stderr.String(), 4))
	}
	return nil
}

// stagingDir is the remote directory of one run.
func stagingDir(runID string) string { return stagingRoot + "/" + runID }

// checkStagingPath refuses any path pvelab did not make for runID.
func checkStagingPath(dir, runID string) error {
	clean := path.Clean(dir)
	if runID == "" || strings.ContainsAny(runID, "/ ") || clean != dir ||
		!strings.HasPrefix(clean, stagingRoot+"/") || path.Base(clean) != runID || path.Dir(clean) != stagingRoot {
		return fmt.Errorf("refusing to remove %q: not the staging directory of run %q", dir, runID)
	}
	return nil
}

func (s SSH) MakeStaging(ctx context.Context, runID string) (string, error) {
	dir := stagingDir(runID)
	if err := checkStagingPath(dir, runID); err != nil {
		return "", err
	}
	script := "umask 077 && mkdir -p " + shellQuote(stagingRoot) + " && mkdir " + shellQuote(dir)
	if out, err := s.Command(ctx, script).CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating %s: %w: %s", dir, err, lastLines(string(out), 4))
	}
	return dir, nil
}

func (s SSH) RemoveStaging(ctx context.Context, dir, runID string) error {
	if err := checkStagingPath(dir, runID); err != nil {
		return err
	}
	_, err := s.Run(ctx, "rm", "-rf", "--", dir)
	return err
}

// RemoteSHA256 hashes a file on the node.
func (s SSH) RemoteSHA256(ctx context.Context, file string) (string, error) {
	out, err := s.Run(ctx, "sha256sum", "--", file)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", errors.New("sha256sum printed no hash")
	}
	return fields[0], nil
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
