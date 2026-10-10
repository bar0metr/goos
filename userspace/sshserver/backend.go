//go:build goos_sshcompat

package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"golang.org/x/crypto/ssh"
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
func newBackend(conn net.Conn, cfg Config) (backend, error) {
	signer, err := ssh.NewSignerFromKey(cfg.HostKey)
	if err != nil {
		return nil, err
	}
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
