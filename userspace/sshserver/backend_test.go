//go:build goos_sshcompat && !goos_userspace

package sshserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func sessionConfig(t *testing.T) (Config, ssh.Signer) {
	t.Helper()
	pub, user, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, host, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(user)
	if err != nil {
		t.Fatal(err)
	}
	return Config{HostKey: host, Username: "user", AuthorizedKeys: []ed25519.PublicKey{pub}, MaxAuthAttempts: 5}, signer
}
func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	for _, conn := range []net.Conn{s, c} {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	return s, c
}
func TestAuthenticationRejectsUnauthorizedKey(t *testing.T) {
	cfg, _ := sessionConfig(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(private)
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	_, _, _, err = ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	client.Close()
	if err == nil {
		t.Fatal("unauthorized key authenticated")
	}
	if err = <-done; err == nil {
		t.Fatal("server accepted unauthorized key")
	}
}

type wrongSignature struct {
	public ssh.PublicKey
	signer ssh.Signer
}

func (s wrongSignature) PublicKey() ssh.PublicKey { return s.public }
func (s wrongSignature) Sign(source io.Reader, data []byte) (*ssh.Signature, error) {
	return s.signer.Sign(source, data)
}
func TestAuthenticationRejectsWrongSignatureAfterAcceptedQuery(t *testing.T) {
	cfg, authorized := sessionConfig(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ssh.NewSignerFromKey(private)
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	_, _, _, err = ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{ssh.PublicKeys(wrongSignature{public: authorized.PublicKey(), signer: other})}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	client.Close()
	if err == nil {
		t.Fatal("wrong signature authenticated")
	}
	if err = <-done; err == nil {
		t.Fatal("server accepted wrong signature")
	}
}
func TestAuthenticationRejectsWrongUsername(t *testing.T) {
	cfg, signer := sessionConfig(t)
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	_, _, _, err = ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	client.Close()
	if err == nil {
		t.Fatal("root authenticated")
	}
	if err = <-done; err == nil {
		t.Fatal("server accepted root")
	}
}
func TestAuthenticationUsesAnyAuthorizedKey(t *testing.T) {
	cfg, signer := sessionConfig(t)
	for i := 0; i < 63; i++ {
		key, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AuthorizedKeys = append([]ed25519.PublicKey{key}, cfg.AuthorizedKeys...)
	}
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	c, _, _, err := ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if s.User() != "user" {
		t.Fatal(s.User())
	}
}

type failedRandom struct{}

func (failedRandom) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestHandshakeFailsOnEntropyError(t *testing.T) {
	cfg, signer := sessionConfig(t)
	cfg.Rand = failedRandom{}
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	_, _, _, err = ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err == nil {
		t.Fatal("entropy failure authenticated")
	}
	if err = <-done; err != io.ErrUnexpectedEOF {
		t.Fatalf("entropy error = %v", err)
	}
}
func TestOpenSSHInteractive(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	cfg, _ := sessionConfig(t)
	private := ed25519.NewKeyFromSeed(make([]byte, 32))
	cfg.AuthorizedKeys = []ed25519.PublicKey{private.Public().(ed25519.PublicKey)}
	block, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/Volumes/Data/tmp", "goos-ssh-openssh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	keyPath := filepath.Join(dir, "identity")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		server, err := New(conn, cfg)
		if err != nil {
			done <- err
			return
		}
		if err = server.Handshake(); err != nil {
			done <- err
			return
		}
		input := make([]byte, MaxCiphertext)
		for {
			event, err := server.Poll()
			if err != nil {
				done <- err
				return
			}
			if event.Kind == Shell {
				if _, err = server.Write([]byte("OPENSSH_SHELL_OK\r\n")); err == nil {
					err = server.Exit(0)
				}
				if err != nil {
					done <- err
					return
				}
			}
			if event.Kind == Close {
				// Complete the channel close handshake before closing TCP. Drain
				// the peer's final disconnect to avoid an unread-data TCP reset.
				if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
					done <- err
					return
				}
				_, err = io.CopyN(io.Discard, conn, MaxCiphertext)
				if err == io.EOF {
					err = nil
				}
				done <- err
				return
			}
			if event.Kind == NeedInput {
				n, err := conn.Read(input[:server.NeedInput()])
				if err != nil {
					done <- err
					return
				}
				if _, err = server.Feed(input[:n]); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	args := []string{"-F", "/dev/null", "-tt", "-p", port, "-i", keyPath, "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "KexAlgorithms=curve25519-sha256", "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PubkeyAcceptedAlgorithms=ssh-ed25519", "-o", "Ciphers=aes128-gcm@openssh.com", "user@127.0.0.1"}
	scriptPath, err := exec.LookPath("script")
	if err != nil {
		t.Skip("host PTY script utility unavailable")
	}
	var command *exec.Cmd
	if runtime.GOOS == "darwin" {
		command = exec.Command(scriptPath, append([]string{"-q", "/dev/null", "/bin/sh", "-c", "stty rows 24 cols 80; exec \"$@\"", "goos-ssh-test", sshPath}, args...)...)
	} else if runtime.GOOS == "linux" {
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		line := "stty rows 24 cols 80; exec " + quote(sshPath)
		for _, arg := range args {
			line += " " + quote(arg)
		}
		command = exec.Command(scriptPath, "-q", "-e", "-c", line, "/dev/null")
	} else {
		t.Skip("host PTY test supports macOS and Linux")
	}
	command.Env = append(os.Environ(), "TERM=xterm")
	command.Stdin = strings.NewReader("")
	var output, stderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &stderr
	if err = command.Run(); err != nil {
		t.Fatalf("OpenSSH: %v: stderr=%q output=%q", err, stderr.String(), output.String())
	}
	if err = <-done; err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "OPENSSH_SHELL_OK") {
		t.Fatalf("OpenSSH output: %q", output.String())
	}
}
