// Package identityd implements the M4 identity service: a single-owner
// dispatch and availability state machine over the host-tested identity store
// (ADR 0233). This file imports only the identity store and the identity
// codec, never kernel IPC, VFS, or arena code, so it runs under plain
// `go test`. The native service in userspace/cmd/identityd wires the IPC and
// VFS session around it.
package identityd

import (
	"errors"

	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

// Service owns the identity store and the availability state identityd
// reports. It is a single mutation owner: every write goes through HandleRequest
// or the Provision/Reset methods, never around them.
type Service struct {
	store *identity.Store
	state identityproto.State
}

// New returns a fresh, unprovisioned service with no store.
func New() *Service {
	return &Service{state: identityproto.StateUnprovisioned}
}

// State returns the availability state (Available, Unprovisioned, Unavailable).
func (s *Service) State() identityproto.State {
	if s == nil {
		return identityproto.StateUnprovisioned
	}
	return s.state
}

// Adopt installs an already-decoded store and availability state. It is the
// persistence layer's entry point: Load handles raw snapshot bytes, while the
// persistence layer decodes first and reports the state to adopt.
func (s *Service) Adopt(store *identity.Store, state identityproto.State) {
	s.store = store
	s.state = state
}

// Load decodes and validates a snapshot and sets the availability state:
// a nil snapshot is missing (Unprovisioned), invalid data is corruption
// (Unavailable), and a valid snapshot is Available. The returned error reports
// the corruption reason.
func (s *Service) Load(data []byte) error {
	if data == nil {
		s.Adopt(nil, identityproto.StateUnprovisioned)
		return nil
	}
	store, err := identity.Decode(data)
	if err != nil {
		s.Adopt(nil, identityproto.StateUnavailable)
		return err
	}
	s.Adopt(store, identityproto.StateAvailable)
	return nil
}

// Snapshot encodes the current store as the canonical snapshot. It fails when
// there is no store (Unprovisioned/Unavailable).
func (s *Service) Snapshot() ([]byte, error) {
	if s.store == nil {
		return nil, identity.ErrUnavailable
	}
	return identity.Encode(s.store)
}

// Counts returns the active account, group, and total key counts.
func (s *Service) Counts() (accounts, groups, keys uint32) {
	if s.store == nil {
		return 0, 0, 0
	}
	list := s.store.Accounts()
	for i := range list {
		keys += uint32(list[i].KeyCount)
	}
	return uint32(len(list)), uint32(len(s.store.Groups())), keys
}

// Provision creates an empty store and marks the service Available. It is the
// explicit provisioning step; it writes an empty valid snapshot and never
// regenerates corrupt data over.
func (s *Service) Provision() error {
	s.store = identity.NewStore()
	s.state = identityproto.StateAvailable
	return nil
}

// Reset destructively clears the store to an empty Available service. It is
// the recovery action for corrupt or unusable data and requires an explicit
// administrator action.
func (s *Service) Reset() {
	s.store = identity.NewStore()
	s.state = identityproto.StateAvailable
}

// Records returns every account or group record in deterministic order
// (account/group ID ascending).
func (s *Service) Records(kind identityproto.RecordKind) []identityproto.Record {
	if s.store == nil {
		return nil
	}
	switch kind {
	case identityproto.RecordAccount:
		accounts := s.store.Accounts()
		records := make([]identityproto.Record, 0, len(accounts))
		for i := range accounts {
			records = append(records, s.accountRecord(&accounts[i]))
		}
		return records
	case identityproto.RecordGroup:
		groups := s.store.Groups()
		records := make([]identityproto.Record, 0, len(groups))
		for i := range groups {
			records = append(records, groupRecord(&groups[i]))
		}
		return records
	}
	return nil
}

// AuthorizeKey reports whether an enabled account with the exact name holds the
// key, plus the Result the caller should return. Only (true, ResultOK) is an
// authorization; a disabled account, a wrong key, a missing account, a bad
// key length, and a non-available store all deny.
func (s *Service) AuthorizeKey(username string, key []byte) (bool, identityproto.Result) {
	if s.state != identityproto.StateAvailable {
		if s.state == identityproto.StateUnprovisioned {
			return false, identityproto.ResultUnprovisioned
		}
		return false, identityproto.ResultUnavailable
	}
	if len(key) != identity.KeySize {
		return false, identityproto.ResultInvalid
	}
	var k identity.Key
	copy(k[:], key)
	ok, err := s.store.AuthorizeKey(username, k)
	if err != nil {
		return false, resultFor(err)
	}
	if !ok {
		// An enabled account without this key denies without an error.
		return false, identityproto.ResultDenied
	}
	return true, identityproto.ResultOK
}

// HandleRequest dispatches one decoded administration or lookup request to the
// store and returns the reply. AdminStatus, AdminProvision and AdminReset work
// in every state; data and lookup operations are refused with
// ResultUnprovisioned or ResultUnavailable while the store is not Available.
func (s *Service) HandleRequest(r *identityproto.Request) identityproto.Reply {
	reply := identityproto.Reply{Opcode: r.Opcode, State: identityproto.StateAvailable}
	switch r.Opcode {
	case identityproto.AdminStatus:
		accounts, groups, keys := s.Counts()
		return identityproto.Reply{
			Opcode:   identityproto.AdminStatus,
			Result:   identityproto.ResultOK,
			State:    s.State(),
			Accounts: accounts,
			Groups:   groups,
			Keys:     keys,
		}
	case identityproto.AdminProvision:
		if err := s.Provision(); err != nil {
			return replyResult(reply, identityproto.ResultUnavailable)
		}
		return replyResult(reply, identityproto.ResultOK)
	case identityproto.AdminReset:
		s.Reset()
		return replyResult(reply, identityproto.ResultOK)
	}

	// Data and lookup operations require an available store.
	switch s.state {
	case identityproto.StateUnprovisioned:
		return replyResult(reply, identityproto.ResultUnprovisioned)
	case identityproto.StateUnavailable:
		return replyResult(reply, identityproto.ResultUnavailable)
	}

	switch r.Opcode {
	case identityproto.AdminListAccounts:
		reply.Records = s.Records(identityproto.RecordAccount)
		reply.Result = identityproto.ResultOK
	case identityproto.AdminListGroups:
		reply.Records = s.Records(identityproto.RecordGroup)
		reply.Result = identityproto.ResultOK
	case identityproto.AdminGetAccount, identityproto.LookupGetAccount:
		account, err := s.store.AccountByName(string(r.Name))
		if err != nil {
			reply.Result = resultFor(err)
		} else {
			reply.Result = identityproto.ResultOK
			reply.Records = []identityproto.Record{s.accountRecord(account)}
		}
	case identityproto.AdminGetGroup:
		group, err := s.store.GroupByName(string(r.Name))
		if err != nil {
			reply.Result = resultFor(err)
		} else {
			reply.Result = identityproto.ResultOK
			reply.Records = []identityproto.Record{groupRecord(group)}
		}
	case identityproto.AdminAddAccount:
		_, err := s.store.CreateAccount(string(r.Name))
		reply.Result = resultFor(err)
	case identityproto.AdminDeleteAccount:
		reply.Result = resultFor(s.store.DeleteAccount(string(r.Name)))
	case identityproto.AdminSetEnabled:
		reply.Result = resultFor(s.store.SetEnabled(string(r.Name), r.Enabled))
	case identityproto.AdminSetUID:
		reply.Result = resultFor(s.store.SetUID(string(r.Name), r.UID))
	case identityproto.AdminSetGID:
		reply.Result = resultFor(s.store.SetPrimaryGID(string(r.Name), r.GID))
	case identityproto.AdminSetHome:
		reply.Result = resultFor(s.store.SetHome(string(r.Name), string(r.Home)))
	case identityproto.AdminSetGroups:
		reply.Result = resultFor(s.store.SetGroups(string(r.Name), r.IDs))
	case identityproto.AdminAddKey:
		var key identity.Key
		copy(key[:], r.Key)
		reply.Result = resultFor(s.store.AddKey(string(r.Name), key))
	case identityproto.AdminRemoveKey:
		var fingerprint [identity.FingerprintSize]byte
		copy(fingerprint[:], r.Fingerprint)
		reply.Result = resultFor(s.store.RemoveKey(string(r.Name), fingerprint))
	case identityproto.AdminAddGroup:
		_, err := s.store.CreateGroup(string(r.Name))
		reply.Result = resultFor(err)
	case identityproto.AdminDeleteGroup:
		reply.Result = resultFor(s.store.DeleteGroup(string(r.Name)))
	case identityproto.AdminSetMembers:
		reply.Result = resultFor(s.store.SetMembers(string(r.Name), r.IDs))
	case identityproto.LookupAuthorizeKey:
		_, result := s.AuthorizeKey(string(r.Name), r.Key)
		reply.Result = result
	default:
		reply.Result = identityproto.ResultInvalid
	}
	return reply
}

// resultFor maps a store sentinel error to its identityproto result, and a nil
// error to ResultOK.
func resultFor(err error) identityproto.Result {
	switch {
	case err == nil:
		return identityproto.ResultOK
	case errors.Is(err, identity.ErrNotFound):
		return identityproto.ResultNotFound
	case errors.Is(err, identity.ErrExists):
		return identityproto.ResultExists
	case errors.Is(err, identity.ErrInvalid):
		return identityproto.ResultInvalid
	case errors.Is(err, identity.ErrExhausted):
		return identityproto.ResultExhausted
	case errors.Is(err, identity.ErrConflict):
		return identityproto.ResultConflict
	case errors.Is(err, identity.ErrUnavailable):
		return identityproto.ResultUnavailable
	case errors.Is(err, identity.ErrDisabled):
		return identityproto.ResultDenied
	default:
		return identityproto.ResultUnavailable
	}
}

// replyResult sets reply.Result and returns it.
func replyResult(reply identityproto.Reply, result identityproto.Result) identityproto.Reply {
	reply.Result = result
	return reply
}

// accountRecord builds the bounded account record for get and list replies.
// GroupID is the account's internal primary group ID; GID carries that
// primary group's Unix GID, resolved by scanning the group list.
func (s *Service) accountRecord(a *identity.Account) identityproto.Record {
	gid, _ := s.primaryGroupGID(a)
	return identityproto.Record{
		Kind:      identityproto.RecordAccount,
		AccountID: a.ID,
		GroupID:   a.PrimaryGID,
		UID:       a.UID,
		GID:       gid,
		Enabled:   a.Enabled,
		KeyCount:  a.KeyCount,
		Name:      []byte(a.NameString()),
		Home:      []byte(a.HomePath()),
	}
}

// groupRecord builds the bounded group record for get and list replies.
func groupRecord(g *identity.Group) identityproto.Record {
	return identityproto.Record{
		Kind:    identityproto.RecordGroup,
		GroupID: g.ID,
		GID:     g.GID,
		Name:    []byte(g.NameString()),
	}
}

// primaryGroupGID resolves an account's primary group to its Unix GID by
// scanning the group list. Accounts carry only the internal GroupID; the Unix
// GID lives on the group record.
func (s *Service) primaryGroupGID(a *identity.Account) (uint32, bool) {
	if s.store == nil || a == nil {
		return 0, false
	}
	for _, g := range s.store.Groups() {
		if g.ID == a.PrimaryGID {
			return g.GID, true
		}
	}
	return 0, false
}
