// Package identityd implements the M4 identity service: a single-owner
// dispatch and availability state machine over the host-tested identity store
// (ADR 0233). This file imports only the identity store and the identity
// codec, never kernel IPC, VFS, or arena code, so it runs under plain
// `go test`. The native service in userspace/cmd/identityd wires the IPC and
// VFS session around it.
package identityd

import (
	"errors"
	"sort"

	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

// EpochSource returns a fresh 128-bit database epoch from the trusted
// provisioning/recovery path (for example the OS random source). It must fail
// if secure generation fails. A returned zero epoch is rejected: the library's
// Decode refuses a zero epoch, and provisioning/recovery never mint one.
type EpochSource func() ([16]byte, error)

// Service is the single mutation owner: every write goes through HandleRequest
// (or the internal provision/reset paths), which clone the committed store,
// apply the mutation, validate and encode it, persist it durably, and only
// then publish it. Reads and authorization observe only the last confirmed
// committed view, or deny while unavailable. No background writes exist.
type Service struct {
	store   *identity.Store
	state   identityproto.State
	persist *Persist
	epoch   EpochSource
}

// New returns an unprovisioned service that persists to path through files and
// obtains new database epochs from epoch (the trusted provisioning/recovery
// path). epoch may be nil only if the service is never provisioned or reset
// from corruption; those paths then fail closed with Unavailable.
func New(files Files, path string, epoch EpochSource) *Service {
	return &Service{
		persist: NewPersist(files, path),
		epoch:   epoch,
		state:   identityproto.StateUnprovisioned,
	}
}

// Open reconciles any stale temporary file and loads the durable snapshot into
// the service, setting the availability state: missing → Unprovisioned, valid
// → Available, corrupt or ambiguous → Unavailable. It never regenerates data.
func (s *Service) Open() error {
	if s.persist == nil {
		s.store = nil
		s.state = identityproto.StateUnavailable
		return ErrUncertain
	}
	store, err := s.persist.Open()
	switch {
	case err == nil:
		s.store = store
		s.state = identityproto.StateAvailable
	case errors.Is(err, ErrNotFound):
		s.store = nil
		s.state = identityproto.StateUnprovisioned
	default:
		s.store = nil
		s.state = identityproto.StateUnavailable
	}
	return err
}

// State returns the availability state (Available, Unprovisioned, Unavailable).
func (s *Service) State() identityproto.State {
	if s == nil {
		return identityproto.StateUnprovisioned
	}
	return s.state
}

// Counts returns the active account, group, and total key counts. It reports
// zeros unless the store is Available.
func (s *Service) Counts() (accounts, groups, keys uint32) {
	if s.store == nil || s.state != identityproto.StateAvailable {
		return 0, 0, 0
	}
	list := s.store.Accounts()
	for i := range list {
		keys += uint32(list[i].KeyCount)
	}
	return uint32(len(list)), uint32(len(s.store.Groups())), keys
}

// Snapshot encodes the current committed store as the canonical snapshot. It
// fails when there is no store (Unprovisioned/Unavailable).
func (s *Service) Snapshot() ([]byte, error) {
	if s.store == nil {
		return nil, identity.ErrUnavailable
	}
	return identity.Encode(s.store)
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
			records = append(records, s.groupRecord(&groups[i]))
		}
		return records
	}
	return nil
}

// AuthorizeKey reports whether an enabled account with the exact name holds the
// key, plus the Result the caller should return. It reads only the committed
// snapshot. Only (true, ResultOK) is an authorization; a disabled account, a
// wrong key, a missing account, a bad key length, and a non-available store
// all deny.
func (s *Service) AuthorizeKey(username string, key []byte) (bool, identityproto.Result) {
	if s.state != identityproto.StateAvailable || s.store == nil {
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

// HandleRequest dispatches one decoded administration or lookup request and
// returns the reply carrying the actual availability state. Status, Provision
// and Reset work in every state per the lifecycle table; data mutations and
// key authorization/lookup are refused with ResultUnprovisioned or
// ResultUnavailable unless the store is Available. A lookup opcode never
// mutates.
func (s *Service) HandleRequest(r *identityproto.Request) identityproto.Reply {
	if r == nil {
		return identityproto.Reply{Opcode: 0, Result: identityproto.ResultInvalid, State: s.State()}
	}
	reply := identityproto.Reply{Opcode: r.Opcode, State: s.State()}

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
		reply.Result = resultFor(s.provision())
		reply.State = s.State()
		return reply
	case identityproto.AdminReset:
		reply.Result = resultFor(s.reset())
		reply.State = s.State()
		return reply
	}

	// Data mutations and key authorization/lookup require an Available store.
	if s.state != identityproto.StateAvailable || s.store == nil {
		if s.state == identityproto.StateUnprovisioned {
			reply.Result = identityproto.ResultUnprovisioned
		} else {
			reply.Result = identityproto.ResultUnavailable
		}
		reply.State = s.State()
		return reply
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
			reply.Records = []identityproto.Record{s.groupRecord(group)}
		}
	case identityproto.LookupAuthorizeKey:
		_, result := s.AuthorizeKey(string(r.Name), r.Key)
		reply.Result = result
	case identityproto.LookupAuthorizeAndResolve:
		// One coherent answer from the same committed snapshot: the key
		// authorization and the resolved account record are read together, so
		// admission never authorizes one snapshot and resolves another.
		if _, result := s.AuthorizeKey(string(r.Name), r.Key); result != identityproto.ResultOK {
			reply.Result = result
		} else if account, err := s.store.AccountByName(string(r.Name)); err != nil {
			reply.Result = resultFor(err)
		} else {
			reply.Result = identityproto.ResultOK
			reply.Records = []identityproto.Record{s.accountRecord(account)}
		}

	case identityproto.AdminAddAccount:
		reply.Result = s.mutate(func(st *identity.Store) error {
			_, err := st.CreateAccount(string(r.Name))
			return err
		})
	case identityproto.AdminDeleteAccount:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.DeleteAccount(string(r.Name)) })
	case identityproto.AdminSetEnabled:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetEnabled(string(r.Name), r.Enabled) })
	case identityproto.AdminSetUID:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetUID(string(r.Name), r.UID) })
	case identityproto.AdminSetGID:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetPrimaryGID(string(r.Name), r.GID) })
	case identityproto.AdminSetHome:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetHome(string(r.Name), string(r.Home)) })
	case identityproto.AdminSetGroups:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetGroups(string(r.Name), r.IDs) })
	case identityproto.AdminAddKey:
		var key identity.Key
		copy(key[:], r.Key)
		reply.Result = s.mutate(func(st *identity.Store) error { return st.AddKey(string(r.Name), key) })
	case identityproto.AdminRemoveKey:
		var fingerprint [identity.FingerprintSize]byte
		copy(fingerprint[:], r.Fingerprint)
		reply.Result = s.mutate(func(st *identity.Store) error { return st.RemoveKey(string(r.Name), fingerprint) })
	case identityproto.AdminAddGroup:
		reply.Result = s.mutate(func(st *identity.Store) error {
			_, err := st.CreateGroup(string(r.Name))
			return err
		})
	case identityproto.AdminDeleteGroup:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.DeleteGroup(string(r.Name)) })
	case identityproto.AdminSetMembers:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetMembers(string(r.Name), r.IDs) })
	case identityproto.AdminSetGroupGID:
		reply.Result = s.mutate(func(st *identity.Store) error { return st.SetGroupGID(string(r.Name), r.GID) })
	case identityproto.AdminImportLegacy:
		keys, ok := identityproto.ParseImportKeys(r.Key)
		if !ok {
			reply.Result = identityproto.ResultInvalid
			break
		}
		reply.Result = s.mutate(func(st *identity.Store) error {
			return st.ImportLegacy(string(r.Name), string(r.Home), keys)
		})
	default:
		reply.Result = identityproto.ResultInvalid
	}
	reply.State = s.State()
	return reply
}

// mutate applies fn to a deep clone of the committed store, validates and
// encodes the candidate, persists it durably, and only then publishes it.
// Invalid, denied, or exhausted mutations never call persistence.
func (s *Service) mutate(fn func(*identity.Store) error) identityproto.Result {
	candidate := s.store.Clone()
	if err := fn(candidate); err != nil {
		return resultFor(err)
	}
	data, err := identity.Encode(candidate)
	if err != nil {
		return resultFor(err)
	}
	if err := identity.Validate(data); err != nil {
		return resultFor(err)
	}
	if err := s.commitStore(candidate, data); err != nil {
		return resultFor(err)
	}
	return identityproto.ResultOK
}

// provision commits an empty database with a fresh epoch (from the trusted
// source) only from Unprovisioned. It never overwrites a valid (Available) or
// corrupt (Unavailable) store.
func (s *Service) provision() error {
	switch s.state {
	case identityproto.StateAvailable:
		return identity.ErrConflict
	case identityproto.StateUnavailable:
		return identity.ErrUnavailable
	}
	epoch, err := s.newEpoch()
	if err != nil {
		return err
	}
	return s.persistEmpty(identity.NewStoreWithEpoch(epoch))
}

// reset clears the store. From Available it preserves the epoch and all
// high-water marks (internal ID and numeric counters) and advances the
// committed revision; from Unavailable it performs deliberate recovery with a
// fresh epoch and fresh counters; from Unprovisioned it is a conflict because
// provisioning is the correct first step.
func (s *Service) reset() error {
	switch s.state {
	case identityproto.StateUnprovisioned:
		return identity.ErrConflict
	case identityproto.StateUnavailable:
		epoch, err := s.newEpoch()
		if err != nil {
			return err
		}
		return s.persistEmpty(identity.NewStoreWithEpoch(epoch))
	case identityproto.StateAvailable:
		candidate := s.store.Clone()
		for _, a := range candidate.Accounts() {
			if err := candidate.DeleteAccount(a.NameString()); err != nil {
				return err
			}
		}
		for _, g := range candidate.Groups() {
			if err := candidate.DeleteGroup(g.NameString()); err != nil {
				return err
			}
		}
		data, err := identity.Encode(candidate)
		if err != nil {
			return err
		}
		if err := identity.Validate(data); err != nil {
			return err
		}
		return s.commitStore(candidate, data)
	}
	return identity.ErrUnavailable
}

// persistEmpty encodes and durably commits an empty store (provision/recovery).
func (s *Service) persistEmpty(store *identity.Store) error {
	data, err := identity.Encode(store)
	if err != nil {
		return err
	}
	if err := identity.Validate(data); err != nil {
		return err
	}
	return s.commitStore(store, data)
}

// commitStore persists the encoded candidate and publishes it only on a
// durable outcome. A definite pre-replace failure retains the old committed
// state; an uncertain or final-sync failure fails closed into Unavailable.
func (s *Service) commitStore(candidate *identity.Store, data []byte) error {
	if s.persist == nil {
		s.store = nil
		s.state = identityproto.StateUnavailable
		return ErrUncertain
	}
	switch err := s.persist.Save(data); {
	case err == nil:
		s.store = candidate
		s.state = identityproto.StateAvailable
		return nil
	case errors.Is(err, ErrUncertain):
		// Ambiguous replace or failed final sync: no success, no rollback by
		// guessing. Reconcile from the durable snapshot on a fresh lifetime.
		s.store = nil
		s.state = identityproto.StateUnavailable
		return err
	default:
		// Definite pre-replace failure: old committed store retained.
		return err
	}
}

// newEpoch obtains a fresh database epoch from the trusted source, failing
// closed when generation fails or returns a zero epoch.
func (s *Service) newEpoch() ([16]byte, error) {
	if s.epoch == nil {
		return [16]byte{}, identity.ErrUnavailable
	}
	epoch, err := s.epoch()
	if err != nil {
		return [16]byte{}, err
	}
	if epoch == ([16]byte{}) {
		return [16]byte{}, identity.ErrUnavailable
	}
	return epoch, nil
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

// accountRecord builds the bounded account record for get and list replies,
// carrying the coherent committed principal view: stable AccountID, the
// database epoch and committed revision, the Unix UID, the primary group's
// Unix GID (with its internal GroupID), the supplementary group Unix GIDs
// sorted ascending, home, and enabled state.
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
		Epoch:     s.store.Epoch(),
		Revision:  s.store.Sequence(),
		Groups:    s.supplementaryGIDs(a),
		Name:      []byte(a.NameString()),
		Home:      []byte(a.HomePath()),
	}
}

// groupRecord builds the bounded group record for get and list replies,
// carrying the same committed epoch and revision as the account records.
func (s *Service) groupRecord(g *identity.Group) identityproto.Record {
	return identityproto.Record{
		Kind:     identityproto.RecordGroup,
		GroupID:  g.ID,
		GID:      g.GID,
		Epoch:    s.store.Epoch(),
		Revision: s.store.Sequence(),
		Name:     []byte(g.NameString()),
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

// supplementaryGIDs resolves an account's supplementary internal GroupIDs to
// their Unix GIDs, sorted ascending, for the coherent principal view.
func (s *Service) supplementaryGIDs(a *identity.Account) []uint32 {
	if s.store == nil || a == nil || a.GroupCount == 0 {
		return nil
	}
	groups := s.store.Groups()
	gids := make([]uint32, 0, int(a.GroupCount))
	for i := 0; i < int(a.GroupCount); i++ {
		for _, g := range groups {
			if g.ID == a.Groups[i] {
				gids = append(gids, g.GID)
				break
			}
		}
	}
	sort.Slice(gids, func(i, j int) bool { return gids[i] < gids[j] })
	return gids
}
