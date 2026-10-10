//go:build goos_sshcompat

package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
)

type pumpBackend struct {
	pump    *ssh.Pump
	memory  pumpMemory
	data    []byte
	failure stableError
}
type stableError struct {
	message [256]byte
	size    int
}

func (e *stableError) Error() string { return string(e.message[:e.size]) }
func (b *pumpBackend) stable(err error) error {
	if err == nil {
		return nil
	}
	b.failure.size = copy(b.failure.message[:], err.Error())
	return &b.failure
}

var errInvalidHostSignature = errors.New("sshserver: invalid host signature")

type hostSigner struct {
	public ssh.PublicKey
	key    ed25519.PublicKey
	sign   func([]byte) ([]byte, error)
}

func (s hostSigner) PublicKey() ssh.PublicKey { return s.public }
func (s hostSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	// The pinned curve25519-sha256 KEX supplies H without another hash.
	if len(data) != sha256.Size {
		return nil, errInvalidHostSignature
	}
	signature, err := s.sign(data)
	if err != nil {
		return nil, err
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(s.key, data, signature) {
		return nil, errInvalidHostSignature
	}
	return &ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: signature}, nil
}

func newBackend(conn net.Conn, cfg Config) (backend, error) {
	hostPublic := append(ed25519.PublicKey(nil), cfg.HostPublicKey...)
	public, err := ssh.NewPublicKey(hostPublic)
	if err != nil {
		return nil, err
	}
	signer := hostSigner{public: public, key: hostPublic, sign: cfg.SignHost}
	keys := make([][]byte, len(cfg.AuthorizedKeys))
	for i, key := range cfg.AuthorizedKeys {
		public, err := ssh.NewPublicKey(ed25519.PublicKey(key))
		if err != nil {
			return nil, err
		}
		keys[i] = public.Marshal()
	}
	source := cfg.Rand
	if source == nil {
		source = rand.Reader
	}
	source = nativeRandom(source)
	server := &ssh.ServerConfig{Config: ssh.Config{Rand: source}, MaxAuthTries: cfg.MaxAuthAttempts, PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		allowed := 0
		blob := key.Marshal()
		for _, candidate := range keys {
			allowed |= subtle.ConstantTimeCompare(candidate, blob)
		}
		if c.User() != cfg.Username || key.Type() != ssh.KeyAlgoED25519 || allowed != 1 {
			return nil, ssh.ErrNoAuth
		}
		return nil, nil
	}}
	if cfg.Banner != "" {
		banner := cfg.Banner
		server.BannerCallback = func(ssh.ConnMetadata) string { return banner }
	}
	server.AddHostKey(signer)
	b := &pumpBackend{memory: newPumpMemory(), data: make([]byte, MaxData)}
	p, err := ssh.NewPump(conn, ssh.PumpConfig{Server: server, Memory: &b.memory})
	if err != nil {
		return nil, err
	}
	b.pump = p
	return b, nil
}
func (b *pumpBackend) Handshake() error              { return b.pump.Handshake() }
func (b *pumpBackend) Feed(data []byte) (int, error) { return b.pump.Feed(data) }
func (b *pumpBackend) Poll() (Event, error) {
	scope := b.memory.packet()
	defer scope.leave()
	e, err := b.pump.Poll()
	if err != nil {
		return Event{}, b.stable(err)
	}
	// Ciphertext, strings and crypto objects never escape the transient region.
	n := copy(b.data, e.Data)
	return Event{Kind: EventKind(e.Kind), Data: b.data[:n], Term: e.Term, TermLen: e.TermLen, Rows: e.Rows, Columns: e.Columns}, nil
}
func (b *pumpBackend) Write(data []byte) (int, error) {
	scope := b.memory.packet()
	defer scope.leave()
	n, err := b.pump.Write(data)
	return n, b.stable(err)
}
func (b *pumpBackend) Exit(status uint32) error {
	scope := b.memory.packet()
	defer scope.leave()
	return b.stable(b.pump.Exit(status))
}
func (b *pumpBackend) Close() error             { return b.pump.Close() }
func (b *pumpBackend) User() string             { return b.pump.User() }
func (b *pumpBackend) NeedInput() int           { return b.pump.NeedInput() }
func (b *pumpBackend) KeyExchangePending() bool { return b.pump.KeyExchangePending() }
