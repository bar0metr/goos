package identity

import (
	"bytes"
	"crypto/subtle"
	"sort"
)

const maxUint64 = ^uint64(0)

// --- lookups ---

func (s *Store) findAccount(name string) (int, bool) {
	for i := range s.accounts {
		if s.accounts[i].NameString() == name {
			return i, true
		}
	}
	return -1, false
}

func (s *Store) findGroup(name string) (int, bool) {
	for i := range s.groups {
		if s.groups[i].NameString() == name {
			return i, true
		}
	}
	return -1, false
}

func (s *Store) findAccountByUID(uid uint32) (int, bool) {
	for i := range s.accounts {
		if s.accounts[i].UID == uid {
			return i, true
		}
	}
	return -1, false
}

func (s *Store) findGroupByGID(gid uint32) (int, bool) {
	for i := range s.groups {
		if s.groups[i].GID == gid {
			return i, true
		}
	}
	return -1, false
}

func (s *Store) groupIDExists(id uint64) bool {
	for i := range s.groups {
		if s.groups[i].ID == id {
			return true
		}
	}
	return false
}

// groupReferenced reports whether any account holds id as primary or
// supplementary.
func (s *Store) groupReferenced(id uint64) bool {
	for i := range s.accounts {
		a := &s.accounts[i]
		if a.PrimaryGID == id {
			return true
		}
		for j := 0; j < int(a.GroupCount); j++ {
			if a.Groups[j] == id {
				return true
			}
		}
	}
	return false
}

// --- allocation ---

// peekAccountID returns the ID the next account would receive without
// mutating state; the counter is advanced only after full validation.
func (s *Store) peekAccountID() (uint64, error) {
	if s.nextAccountID == maxUint64 {
		return 0, ErrExhausted
	}
	return s.nextAccountID, nil
}

func (s *Store) peekGroupID() (uint64, error) {
	if s.nextGroupID == maxUint64 {
		return 0, ErrExhausted
	}
	return s.nextGroupID, nil
}

// allocUID returns the lowest unused UID at or above AllocBase, skipping
// reserved values. It does not mutate the store.
func (s *Store) allocUID() (uint32, error) {
	for u := AllocBase; u >= AllocBase; u++ {
		if reservedUnix(u) {
			continue
		}
		if _, ok := s.findAccountByUID(u); !ok {
			return u, nil
		}
	}
	return 0, ErrExhausted
}

// allocGID returns the lowest unused GID at or above AllocBase, skipping
// reserved values. It does not mutate the store.
func (s *Store) allocGID() (uint32, error) {
	for g := AllocBase; g >= AllocBase; g++ {
		if reservedUnix(g) {
			continue
		}
		if _, ok := s.findGroupByGID(g); !ok {
			return g, nil
		}
	}
	return 0, ErrExhausted
}

// --- supplementary group array helpers (sorted ascending by GroupID) ---

func groupsHas(groups [MaxSupplementaryGroups]uint64, n uint8, id uint64) bool {
	for i := uint8(0); i < n; i++ {
		if groups[i] == id {
			return true
		}
	}
	return false
}

// groupsInsert inserts id in ascending order. It returns ErrExists if id is
// already present and ErrInvalid if the array is full.
func groupsInsert(groups *[MaxSupplementaryGroups]uint64, n *uint8, id uint64) error {
	cnt := int(*n)
	if cnt >= MaxSupplementaryGroups {
		return ErrInvalid
	}
	i := 0
	for i < cnt && groups[i] < id {
		i++
	}
	if i < cnt && groups[i] == id {
		return ErrExists
	}
	for j := cnt; j > i; j-- {
		groups[j] = groups[j-1]
	}
	groups[i] = id
	*n++
	return nil
}

func groupsRemove(groups *[MaxSupplementaryGroups]uint64, n *uint8, id uint64) {
	cnt := int(*n)
	for i := 0; i < cnt; i++ {
		if groups[i] == id {
			copy(groups[i:cnt], groups[i+1:cnt])
			*n--
			return
		}
	}
}

// --- key array helpers (sorted ascending by fingerprint) ---

func keyLess(a, b Key) bool {
	fa := Fingerprint(a)
	fb := Fingerprint(b)
	return bytes.Compare(fa[:], fb[:]) < 0
}

// keyInsert inserts k in fingerprint order, rejecting duplicates within the
// record and the per-account bound.
func keyInsert(keys *[MaxKeysPerAccount]Key, n *uint8, k Key) error {
	cnt := int(*n)
	if cnt >= MaxKeysPerAccount {
		return ErrExhausted
	}
	for i := 0; i < cnt; i++ {
		if keys[i] == k {
			return ErrExists
		}
	}
	i := 0
	for i < cnt && keyLess(keys[i], k) {
		i++
	}
	for j := cnt; j > i; j-- {
		keys[j] = keys[j-1]
	}
	keys[i] = k
	*n++
	return nil
}

func keyRemoveByFingerprint(keys *[MaxKeysPerAccount]Key, n *uint8, fp [FingerprintSize]byte) bool {
	cnt := int(*n)
	for i := 0; i < cnt; i++ {
		if Fingerprint(keys[i]) == fp {
			copy(keys[i:cnt], keys[i+1:cnt])
			*n--
			return true
		}
	}
	return false
}

func totalKeys(s *Store) int {
	total := 0
	for i := range s.accounts {
		total += int(s.accounts[i].KeyCount)
	}
	return total
}

// --- accounts ---

// CreateAccount creates an enabled account (home empty) and a private primary
// group named after the account, allocating UID and GID independently.
func (s *Store) CreateAccount(name string) (*Account, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	if !ValidName(name) {
		return nil, errInvalid("account name")
	}
	if len(s.accounts) >= MaxAccounts {
		return nil, errExhausted("accounts")
	}
	if len(s.groups) >= MaxGroups {
		return nil, errExhausted("groups")
	}
	if _, ok := s.findAccount(name); ok {
		return nil, errExists("account " + name)
	}
	if _, ok := s.findGroup(name); ok {
		return nil, errExists("group " + name)
	}
	accountID, err := s.peekAccountID()
	if err != nil {
		return nil, err
	}
	uid, err := s.allocUID()
	if err != nil {
		return nil, err
	}
	groupID, err := s.peekGroupID()
	if err != nil {
		return nil, err
	}
	gid, err := s.allocGID()
	if err != nil {
		return nil, err
	}
	account := Account{ID: accountID, UID: uid, PrimaryGID: groupID, Enabled: true}
	account.NameLen = uint8(len(name))
	copy(account.Name[:], name)
	group := Group{ID: groupID, GID: gid}
	group.NameLen = uint8(len(name))
	copy(group.Name[:], name)
	s.nextAccountID++
	s.nextGroupID++
	s.accounts = append(s.accounts, account)
	s.groups = append(s.groups, group)
	s.sequence++
	return &account, nil
}

// DeleteAccount removes the account and, when its primary group is the
// auto-created private group (same name) referenced by no other account,
// removes that group in the same update.
func (s *Store) DeleteAccount(name string) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	primary := s.accounts[i].PrimaryGID
	s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
	if gi, gok := s.findGroup(name); gok && s.groups[gi].ID == primary && !s.groupReferenced(primary) {
		s.groups = append(s.groups[:gi], s.groups[gi+1:]...)
	}
	s.sequence++
	return nil
}

func (s *Store) SetEnabled(name string, enabled bool) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	s.accounts[i].Enabled = enabled
	s.sequence++
	return nil
}

func (s *Store) SetUID(name string, uid uint32) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	if reservedUnix(uid) {
		return errInvalid("reserved UID")
	}
	for j := range s.accounts {
		if j != i && s.accounts[j].UID == uid {
			return errExists("UID")
		}
	}
	s.accounts[i].UID = uid
	s.sequence++
	return nil
}

// SetPrimaryGID resolves gid to a group and sets it as the account's primary.
func (s *Store) SetPrimaryGID(name string, gid uint32) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	gi, ok := s.findGroupByGID(gid)
	if !ok {
		return errNotFound("group GID")
	}
	s.accounts[i].PrimaryGID = s.groups[gi].ID
	s.sequence++
	return nil
}

func (s *Store) SetHome(name, home string) error {
	if s == nil {
		return ErrUnavailable
	}
	if len(home) > MaxHome {
		return errInvalid("home path")
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	s.accounts[i].HomeLen = uint8(len(home))
	copy(s.accounts[i].Home[:], home)
	for j := len(home); j < MaxHome; j++ {
		s.accounts[i].Home[j] = 0
	}
	s.sequence++
	return nil
}

// SetGroups replaces the account's supplementary groups, resolving each GID to
// a GroupID and storing them sorted ascending by GroupID.
func (s *Store) SetGroups(name string, gids []uint32) error {
	if s == nil {
		return ErrUnavailable
	}
	if len(gids) > MaxSupplementaryGroups {
		return errInvalid("supplementary groups")
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	ids := make([]uint64, 0, len(gids))
	for _, gid := range gids {
		gi, ok := s.findGroupByGID(gid)
		if !ok {
			return errNotFound("group GID")
		}
		groupID := s.groups[gi].ID
		for _, existing := range ids {
			if existing == groupID {
				return errInvalid("duplicate supplementary group")
			}
		}
		ids = append(ids, groupID)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	s.accounts[i].Groups = [MaxSupplementaryGroups]uint64{}
	copy(s.accounts[i].Groups[:], ids)
	s.accounts[i].GroupCount = uint8(len(ids))
	s.sequence++
	return nil
}

// AddKey appends key to the account, respecting the per-account and total-key
// bounds and rejecting a duplicate within the record.
func (s *Store) AddKey(name string, key Key) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	if totalKeys(s) >= MaxTotalKeys {
		return errExhausted("total keys")
	}
	if err := keyInsert(&s.accounts[i].Keys, &s.accounts[i].KeyCount, key); err != nil {
		return err
	}
	s.sequence++
	return nil
}

// RemoveKey removes the key whose SHA-256 fingerprint matches.
func (s *Store) RemoveKey(name string, fingerprint [FingerprintSize]byte) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return errNotFound("account " + name)
	}
	if !keyRemoveByFingerprint(&s.accounts[i].Keys, &s.accounts[i].KeyCount, fingerprint) {
		return errNotFound("key")
	}
	s.sequence++
	return nil
}

// --- groups ---

func (s *Store) CreateGroup(name string) (*Group, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	if !ValidName(name) {
		return nil, errInvalid("group name")
	}
	if len(s.groups) >= MaxGroups {
		return nil, errExhausted("groups")
	}
	if _, ok := s.findGroup(name); ok {
		return nil, errExists("group " + name)
	}
	groupID, err := s.peekGroupID()
	if err != nil {
		return nil, err
	}
	gid, err := s.allocGID()
	if err != nil {
		return nil, err
	}
	group := Group{ID: groupID, GID: gid}
	group.NameLen = uint8(len(name))
	copy(group.Name[:], name)
	s.nextGroupID++
	s.groups = append(s.groups, group)
	s.sequence++
	return &group, nil
}

// DeleteGroup removes a group, refusing with a conflict while any account
// references it as primary or supplementary.
func (s *Store) DeleteGroup(name string) error {
	if s == nil {
		return ErrUnavailable
	}
	i, ok := s.findGroup(name)
	if !ok {
		return errNotFound("group " + name)
	}
	if s.groupReferenced(s.groups[i].ID) {
		return errConflict("group " + name)
	}
	s.groups = append(s.groups[:i], s.groups[i+1:]...)
	s.sequence++
	return nil
}

// SetMembers sets exactly which accounts (by UID) hold the group as a
// supplementary group, replacing the previous membership.
func (s *Store) SetMembers(name string, uids []uint32) error {
	if s == nil {
		return ErrUnavailable
	}
	gi, ok := s.findGroup(name)
	if !ok {
		return errNotFound("group " + name)
	}
	target := s.groups[gi].ID
	memberIdx := make([]int, 0, len(uids))
	for _, uid := range uids {
		ai, ok := s.findAccountByUID(uid)
		if !ok {
			return errNotFound("account UID")
		}
		for _, existing := range memberIdx {
			if existing == ai {
				return errInvalid("duplicate member")
			}
		}
		if !groupsHas(s.accounts[ai].Groups, s.accounts[ai].GroupCount, target) &&
			int(s.accounts[ai].GroupCount) >= MaxSupplementaryGroups {
			return errInvalid("supplementary group bound")
		}
		memberIdx = append(memberIdx, ai)
	}
	for i := range s.accounts {
		groupsRemove(&s.accounts[i].Groups, &s.accounts[i].GroupCount, target)
	}
	for _, ai := range memberIdx {
		_ = groupsInsert(&s.accounts[ai].Groups, &s.accounts[ai].GroupCount, target)
	}
	s.sequence++
	return nil
}

// --- queries ---

// AuthorizeKey returns true only when an enabled account with the exact name
// holds the key. A disabled account returns ErrDisabled; a missing account
// returns ErrNotFound; a present, enabled account without the key returns
// (false, nil).
func (s *Store) AuthorizeKey(username string, key Key) (bool, error) {
	if s == nil {
		return false, ErrUnavailable
	}
	i, ok := s.findAccount(username)
	if !ok {
		return false, ErrNotFound
	}
	acct := &s.accounts[i]
	if !acct.Enabled {
		return false, ErrDisabled
	}
	matched := 0
	for j := 0; j < MaxKeysPerAccount; j++ {
		active := subtle.ConstantTimeLessOrEq(j+1, int(acct.KeyCount))
		matched |= active & subtle.ConstantTimeCompare(acct.Keys[j][:], key[:])
	}
	return matched != 0, nil
}

func (s *Store) AccountByName(name string) (*Account, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	i, ok := s.findAccount(name)
	if !ok {
		return nil, ErrNotFound
	}
	a := s.accounts[i]
	return &a, nil
}

func (s *Store) GroupByName(name string) (*Group, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	i, ok := s.findGroup(name)
	if !ok {
		return nil, ErrNotFound
	}
	g := s.groups[i]
	return &g, nil
}

func (s *Store) AccountByUID(uid uint32) (*Account, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	i, ok := s.findAccountByUID(uid)
	if !ok {
		return nil, ErrNotFound
	}
	a := s.accounts[i]
	return &a, nil
}

func (s *Store) GroupByGID(gid uint32) (*Group, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	i, ok := s.findGroupByGID(gid)
	if !ok {
		return nil, ErrNotFound
	}
	g := s.groups[i]
	return &g, nil
}

// Accounts returns a copy of the accounts in AccountID ascending order.
func (s *Store) Accounts() []Account {
	if s == nil {
		return nil
	}
	return append([]Account(nil), s.accounts...)
}

// Groups returns a copy of the groups in GroupID ascending order.
func (s *Store) Groups() []Group {
	if s == nil {
		return nil
	}
	return append([]Group(nil), s.groups...)
}
