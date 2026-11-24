package main

import (
	"bytes"
	"goos/kernel/vfs"
	"goos/userspace/remotepolicy"
)

// PID 1's shared scope-slot allocation contract. The VFS fixes MaxScopes at
// eight, and scope slots cannot be replaced while a namespace lives, so the
// two owners of a slot — remoted's host-key scopes and identityd's database
// subtree — must never overlap (ADR 0233).
const (
	// identityScopeSlot is the slot reserved for the identity database
	// subtree. It is the last slot, so remoted keeps its existing prefix of
	// the slots before it.
	identityScopeSlot = vfs.MaxScopes - 1
	// remoteScopeMax bounds remoted's prefix of scope slots to the slots
	// before identityScopeSlot. rememberRemoteScope and installRemoteScope
	// refuse to cross it, so the identity slot is never stolen.
	remoteScopeMax = identityScopeSlot
)

// remoteScopeAtLimit reports whether count has filled remoted's prefix of
// scope slots and another slot must be refused. The identity slot is the
// first slot past the bound.
func remoteScopeAtLimit(count int) bool { return count >= remoteScopeMax }

// identityAuthority is identityd's authority: confined to root and granted
// Read|Write only on the reserved identity scope slot. Root confinement alone
// leaves the subtree scope denying every ungranted session, and a grant alone
// would let identityd escape its directory, so both are required.
func identityAuthority(root []byte) (vfs.Authority, bool) {
	authority, ok := vfs.RootedAuthority(root)
	if !ok {
		return authority, false
	}
	authority.Confined = true
	authority.Grants[identityScopeSlot] = vfs.Read | vfs.Write
	return authority, true
}

// A Begin correlation is its lease token, so an uncertain Begin can always
// be cancelled without first receiving the successful grant reply.
type remoteKeyLease struct {
	token, revision uint64
	path            [64]byte
	size            int
}

func (g *remoteKeyLease) begin(token, revision uint64, path string) bool {
	if g.token != 0 || token == 0 || revision == 0 || len(path) == 0 || len(path) > len(g.path) {
		return false
	}
	g.token, g.revision, g.size = token, revision, copy(g.path[:], path)
	return true
}
func (g *remoteKeyLease) owns(token, revision uint64, path string) bool {
	return token != 0 && token == g.token && revision == g.revision && bytes.Equal(g.path[:g.size], []byte(path))
}
func (g *remoteKeyLease) end(token uint64) bool {
	if token == 0 || token != g.token {
		return false
	}
	*g = remoteKeyLease{}
	return true
}

type remoteScope struct {
	path      [96]byte
	size      uint8
	subtree   bool
	denied    vfs.Rights
	installed bool
	staging   bool
}

func remoteControllerAuthority(scopes []remoteScope, confined bool) vfs.Authority {
	authority := vfs.Authority{Confined: confined}
	for i, scope := range scopes {
		if !scope.installed || scope.staging {
			continue
		}
		if scope.denied == vfs.Read|vfs.Write {
			authority.Grants[i] = vfs.Read | vfs.Write
		} else {
			authority.Grants[i] = vfs.Read
		}
	}
	return authority
}
func remoteKeyAuthority(scopes []remoteScope, keys string) vfs.Authority {
	authority := vfs.Authority{Confined: true}
	for i, scope := range scopes {
		if !scope.installed || scope.subtree {
			continue
		}
		name := scope.path[:scope.size]
		current := !scope.staging && scope.denied == vfs.Write && bytes.Equal(name, []byte(keys))
		sibling := scope.staging && (bytes.Equal(name, []byte(keys+remotepolicy.KeyStageSuffix)) || bytes.Equal(name, []byte(keys+remotepolicy.KeyPriorSuffix)))
		if current || sibling {
			authority.Grants[i] = vfs.Read | vfs.Write
		}
	}
	return authority
}
