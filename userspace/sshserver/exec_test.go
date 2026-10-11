//go:build goos_sshcompat && !goos_userspace

package sshserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenSSHExecBinaryStreamsAndStatus(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	cfg, _ := sessionConfig(t)
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	cfg.AuthorizedKeys = []ed25519.PublicKey{private.Public().(ed25519.PublicKey)}
	block, err := ssh.MarshalPrivateKey(private, "exec-test")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/Volumes/Data/tmp", "goos-ssh-exec-")
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
	port := listener.Addr().(*net.TCPAddr).Port
	host, err := ssh.NewPublicKey(cfg.HostPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	record := append([]byte(fmt.Sprintf("[127.0.0.1]:%d ", port)), ssh.MarshalAuthorizedKey(host)...)
	if err = os.WriteFile(known, record, 0600); err != nil {
		t.Fatal(err)
	}
	input := []byte{0, 1, 255, 3, 4, '\r', '\n', 'x'}
	expectedOut := []byte{0, 255, 'o', '\n'}
	expectedError := []byte{'e', 0, 255, '\n'}
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
		ciphertext := make([]byte, MaxCiphertext)
		var received []byte
		var command []byte
		for {
			event, err := server.Poll()
			if err != nil {
				done <- err
				return
			}
			switch event.Kind {
			case Exec:
				command = event.Command
				if string(command) != "native-command --bytes" {
					done <- fmt.Errorf("command %q", command)
					return
				}
			case Data:
				received = append(received, event.Data...)
				if err = server.ConsumeInput(uint32(len(event.Data))); err != nil {
					done <- err
					return
				}
			case EOF:
				if !bytes.Equal(received, input) || string(command) != "native-command --bytes" {
					done <- fmt.Errorf("stdin/command changed: %x %q", received, command)
					return
				}
				if n, err := server.Write(expectedOut); err != nil || n != len(expectedOut) {
					done <- fmt.Errorf("stdout %d %v", n, err)
					return
				}
				if n, err := server.WriteStderr(expectedError); err != nil || n != len(expectedError) {
					done <- fmt.Errorf("stderr %d %v", n, err)
					return
				}
				if err = server.Exit(23); err != nil {
					done <- err
					return
				}
			case Close:
				if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
					done <- err
					return
				}
				_, err = io.CopyN(io.Discard, conn, MaxCiphertext)
				if errors.Is(err, io.EOF) {
					err = nil
				}
				done <- err
				return
			case NeedInput:
				count, err := conn.Read(ciphertext[:server.NeedInput()])
				if err != nil {
					done <- err
					return
				}
				if used, err := server.Feed(ciphertext[:count]); err != nil || used != count {
					done <- fmt.Errorf("feed %d %v", used, err)
					return
				}
			}
		}
	}()
	command := exec.Command(sshPath, "-F", "/dev/null", "-T", "-p", fmt.Sprint(port), "-i", keyPath, "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "-o", "LogLevel=ERROR", "-o", "KexAlgorithms=curve25519-sha256", "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PubkeyAcceptedAlgorithms=ssh-ed25519", "-o", "Ciphers=aes128-gcm@openssh.com", "user@127.0.0.1", "native-command --bytes")
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("OpenSSH status %v stderr=%q", err, stderr.Bytes())
	}
	if !bytes.Equal(stdout.Bytes(), expectedOut) || !bytes.Equal(stderr.Bytes(), expectedError) {
		t.Fatalf("stdout=%x stderr=%x", stdout.Bytes(), stderr.Bytes())
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEmptyAuthorizedKeysDenyAll(t *testing.T) {
	cfg, signer := sessionConfig(t)
	cfg.AuthorizedKeys = nil
	server, client := socketPair(t)
	instance, err := New(server, cfg)
	if err != nil {
		t.Fatal("empty deny-all policy refused", err)
	}
	done := make(chan error, 1)
	go func() { done <- instance.Handshake() }()
	public, err := ssh.NewPublicKey(cfg.HostPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(public)})
	client.Close()
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatal("expected deny-all authentication failure", err)
	}
	if err = <-done; err == nil {
		t.Fatal("empty policy accepted authentication")
	}
}
