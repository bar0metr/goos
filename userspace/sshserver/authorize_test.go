//go:build goos_sshcompat && !goos_userspace

package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"goos/userspace/identity"

	"golang.org/x/crypto/ssh"
)

// accountAdapter converts an Ed25519 public key to an identity.Key and defers
// the authorization decision to the store.
func accountAdapter(store *identity.Store) func(username string, key ed25519.PublicKey) (bool, error) {
	return func(username string, key ed25519.PublicKey) (bool, error) {
		if len(key) != ed25519.PublicKeySize {
			return false, nil
		}
		var k identity.Key
		copy(k[:], key)
		return store.AuthorizeKey(username, k)
	}
}

func identityKey(t *testing.T, pub ed25519.PublicKey) identity.Key {
	t.Helper()
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("bad public key size %d", len(pub))
	}
	var k identity.Key
	copy(k[:], pub)
	return k
}

func accountConfig(t *testing.T, store *identity.Store) Config {
	t.Helper()
	hostPublic, host, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		HostPublicKey:   hostPublic,
		SignHost:        func(data []byte) ([]byte, error) { return ed25519.Sign(host, data), nil },
		Username:        "legacy", // unused when AuthorizeKey is set
		MaxAuthAttempts: 5,
		AuthorizeKey:    accountAdapter(store),
	}
}

// handshake runs one server handshake against a client that presents signer as
// user. It returns the server handshake error, the client connection error, and
// the authenticated username reported by the server (empty on failure).
func handshake(t *testing.T, cfg Config, signer ssh.Signer, user string) (serverErr, clientErr error, authenticated string) {
	t.Helper()
	server, client := socketPair(t)
	s, err := New(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	conn, _, _, clientErr := ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if clientErr == nil {
		conn.Close()
	}
	client.Close()
	serverErr = <-done
	authenticated = s.User()
	return
}

func newSigner(t *testing.T) (ed25519.PublicKey, ssh.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pub, signer
}

func TestAuthorizeKeyPositive(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	pub, signer := newSigner(t)
	if err := store.AddKey("alice", identityKey(t, pub)); err != nil {
		t.Fatal(err)
	}
	serverErr, clientErr, authenticated := handshake(t, accountConfig(t, store), signer, "alice")
	if clientErr != nil {
		t.Fatalf("client authentication failed: %v", clientErr)
	}
	if serverErr != nil {
		t.Fatalf("server handshake failed: %v", serverErr)
	}
	if authenticated != "alice" {
		t.Fatalf("authenticated user = %q, want %q", authenticated, "alice")
	}
}

func TestAuthorizeKeyDeniesUnknownKey(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	_, signer := newSigner(t) // key never added to the store
	serverErr, clientErr, authenticated := handshake(t, accountConfig(t, store), signer, "alice")
	if clientErr == nil {
		t.Fatal("unknown key authenticated")
	}
	if serverErr == nil {
		t.Fatal("server accepted unknown key")
	}
	if authenticated != "" {
		t.Fatalf("failed handshake reported user %q", authenticated)
	}
}

func TestAuthorizeKeyIsolation(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	alicePub, _ := newSigner(t)
	bobPub, bobSigner := newSigner(t)
	if err := store.AddKey("alice", identityKey(t, alicePub)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddKey("bob", identityKey(t, bobPub)); err != nil {
		t.Fatal(err)
	}
	// Bob's key must not authorize as alice.
	serverErr, clientErr, _ := handshake(t, accountConfig(t, store), bobSigner, "alice")
	if clientErr == nil {
		t.Fatal("bob's key authenticated as alice")
	}
	if serverErr == nil {
		t.Fatal("server accepted bob's key for alice")
	}
}

func TestAuthorizeKeyDeniesDisabledAccount(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	pub, signer := newSigner(t)
	if err := store.AddKey("alice", identityKey(t, pub)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled("alice", false); err != nil {
		t.Fatal(err)
	}
	serverErr, clientErr, authenticated := handshake(t, accountConfig(t, store), signer, "alice")
	if clientErr == nil {
		t.Fatal("disabled account authenticated")
	}
	if serverErr == nil {
		t.Fatal("server accepted disabled account")
	}
	if authenticated != "" {
		t.Fatalf("failed handshake reported user %q", authenticated)
	}
}

// TestVerifiedKeyIsTheSignedKey proves the server exposes the exact key the
// pinned library verified at signed authentication, not a probed key. Account
// admission uses this identity, so a probe-only key must never surface here.
func TestVerifiedKeyIsTheSignedKey(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	pub, signer := newSigner(t)
	if err := store.AddKey("alice", identityKey(t, pub)); err != nil {
		t.Fatal(err)
	}
	server, client := socketPair(t)
	s, err := New(server, accountConfig(t, store))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	conn, _, _, clientErr := ssh.NewClientConn(client, "pipe", &ssh.ClientConfig{User: "alice", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if clientErr == nil {
		conn.Close()
	}
	client.Close()
	if serverErr := <-done; serverErr != nil || clientErr != nil {
		t.Fatalf("handshake failed: server=%v client=%v", serverErr, clientErr)
	}
	verified, ok := s.VerifiedKey()
	if !ok || len(verified) != ed25519.PublicKeySize || !verified.Equal(pub) {
		t.Fatalf("verified key = %x ok=%v, want the signed key %x", verified, ok, pub)
	}
}

// TestVerifiedKeyAbsentBeforeAuthentication proves the verified key is not
// exposed before a signed authentication completes.
func TestVerifiedKeyAbsentBeforeAuthentication(t *testing.T) {
	store := identity.NewStore()
	if _, err := store.CreateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	server, _ := socketPair(t)
	s, err := New(server, accountConfig(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.VerifiedKey(); ok {
		t.Fatal("verified key exposed before authentication")
	}
}
