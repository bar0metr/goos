// Package sshserver exposes a bounded single-task SSH server. The backend is
// generated from pinned x/crypto/ssh by tools/build-ssh.sh, never installed into
// the host toolchain. Native callers own deadlines and the ciphertext relay.
package sshserver

import (
	"crypto/ed25519"
	"errors"
	"io"
	"net"
)

const MaxData = 16 * 1024
const MaxPacket = 32 * 1024
const MaxCiphertext = MaxPacket + 20

var ErrConfig = errors.New("sshserver: invalid configuration")
var ErrCompatibilityBuild = errors.New("sshserver: build requires generated SSH compatibility module")

type Config struct {
	HostKey         ed25519.PrivateKey
	Username        string
	AuthorizedKeys  []ed25519.PublicKey
	Banner          string
	MaxAuthAttempts int
	// Rand is used only by host tests. Native builds always use cryptosys.Reader.
	Rand io.Reader
}
type EventKind uint8

const (
	NeedInput EventKind = iota
	Progress
	Data
	PTY
	Shell
	Resize
	EOF
	Close
	Rekey
)

// Data is borrowed until the next Poll. Term is inline so events never retain
// transient packet allocations. Use string(event.Term[:event.TermLen]) locally.
type Event struct {
	Kind          EventKind
	Data          []byte
	Term          [64]byte
	TermLen       uint8
	Rows, Columns uint32
}
type backend interface {
	Handshake() error
	Feed([]byte) (int, error)
	Poll() (Event, error)
	Write([]byte) (int, error)
	Exit(uint32) error
	Close() error
	User() string
	NeedInput() int
	KeyExchangePending() bool
}
type Server struct{ backend backend }

func New(conn net.Conn, cfg Config) (*Server, error) {
	if conn == nil || cfg.Username != "user" || len(cfg.HostKey) != ed25519.PrivateKeySize || len(cfg.AuthorizedKeys) == 0 || len(cfg.AuthorizedKeys) > 64 || len(cfg.Banner) > 1024 || cfg.MaxAuthAttempts < 1 || cfg.MaxAuthAttempts > 10 {
		return nil, ErrConfig
	}
	for _, key := range cfg.AuthorizedKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, ErrConfig
		}
	}
	b, err := newBackend(conn, cfg)
	if err != nil {
		return nil, err
	}
	return &Server{backend: b}, nil
}
func (s *Server) Handshake() error            { return s.backend.Handshake() }
func (s *Server) Feed(b []byte) (int, error)  { return s.backend.Feed(b) }
func (s *Server) Poll() (Event, error)        { return s.backend.Poll() }
func (s *Server) Write(b []byte) (int, error) { return s.backend.Write(b) }
func (s *Server) Exit(status uint32) error    { return s.backend.Exit(status) }
func (s *Server) Close() error                { return s.backend.Close() }
func (s *Server) User() string                { return s.backend.User() }
func (s *Server) NeedInput() int              { return s.backend.NeedInput() }
func (s *Server) KeyExchangePending() bool    { return s.backend.KeyExchangePending() }
