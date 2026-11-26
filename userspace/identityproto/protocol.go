// Package identityproto defines GIDNTY1, the versioned IPC codec shared by
// identityd, the administration commands, and PID 1 for the M4 identity
// service (ADR 0233). Messages travel in the kernel/ipc.Message envelope: a
// 256-byte inline payload plus up to four attached handles. Variable data is
// encoded field by field in little-endian with explicit length prefixes and
// bounds, never by serializing Go structs. List replies are multipart with a
// sequence index and a More flag.
package identityproto

import (
	"encoding/binary"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/userspace/identity"
)

const (
	// ProtocolID is the ASCII tag "GIDNTY1".
	ProtocolID = uint64(0x4749444e545931)
	// Version 1 was the initial identity protocol (ADR 0233). Version 2 adds
	// the database epoch, committed revision, and the account's supplementary
	// group Unix GIDs to the record so lookup can return one coherent committed
	// principal view (M4 FIX §5). Version 3 adds AdminSetGroupGID, an explicit
	// Unix GID for a group (ADR 0234). A message of any other version is not
	// this protocol and fails decoding.
	Version = uint16(3)

	requestHeader = 32
	replyHeader   = 40
	controlSize   = 8

	recordFixed = 56

	flagEnabled = 1
	flagMore    = 1

	// Request payload layout.
	requestOpcode  = 0
	requestFlags   = 1
	requestNameLen = 2
	requestHomeLen = 3
	requestIDCount = 4
	requestKeyLen  = 5
	requestFPLen   = 6
	// Byte 7 is reserved.
	requestUID = 8
	requestGID = 12
	// Bytes 16 through requestHeader are reserved.

	// Reply payload layout (list and non-list share the header).
	replyOpcode   = 0
	replyResult   = 1
	replyState    = 2
	replyMore     = 3
	replyCount    = 4
	replyCorr     = 8
	replySequence = 16
	replyAccounts = 24
	replyGroups   = 28
	replyKeys     = 32
	// Bytes 5 through replyCorr and 36 through replyHeader are reserved.

	// Record payload layout.
	recordKind     = 0
	recordEnabled  = 1
	recordNameLen  = 2
	recordHomeLen  = 3
	recordKeyCount = 4
	recordGroupLen = 5
	// Bytes 6 and 7 are reserved.
	recordUID      = 8
	recordGID      = 12
	recordAccount  = 16
	recordGroup    = 24
	recordEpoch    = 32
	recordRevision = 48
)

// Opcode is one identity operation. Control operations run on PID 1's control
// channel and carry one attached session handle; the administration and lookup
// operations run on the session the control operation opened.
type Opcode uint8

const (
	// Control operations, sent by PID 1 on the control channel.
	OpAddAdmin Opcode = iota + 1
	OpAddLookup
	// Administration operations.
	AdminStatus
	AdminProvision
	AdminReset
	AdminListAccounts
	AdminListGroups
	AdminGetAccount
	AdminGetGroup
	AdminAddAccount
	AdminDeleteAccount
	AdminSetEnabled
	AdminSetUID
	AdminSetGID
	AdminSetHome
	AdminSetGroups
	AdminAddKey
	AdminRemoveKey
	AdminAddGroup
	AdminDeleteGroup
	AdminSetMembers
	AdminSetGroupGID
	AdminImportLegacy
	// Lookup operations.
	LookupAuthorizeKey
	LookupGetAccount
	LookupAuthorizeAndResolve
	lastOpcode = LookupAuthorizeAndResolve
)

func isControlOpcode(op Opcode) bool { return op == OpAddAdmin || op == OpAddLookup }
func isDataOpcode(op Opcode) bool    { return op >= AdminStatus && op <= lastOpcode }
func isListOpcode(op Opcode) bool    { return op == AdminListAccounts || op == AdminListGroups }

// Result is a data-operation result code.
type Result uint8

const (
	ResultOK Result = iota
	ResultInvalid
	ResultNotFound
	ResultExists
	ResultExhausted
	ResultUnavailable
	ResultUnprovisioned
	ResultConflict
	ResultDenied
	lastResult = ResultDenied
)

// State is the availability state identityd reports (ADR 0233 §4).
type State uint8

const (
	StateAvailable State = iota
	StateUnprovisioned
	StateUnavailable
	lastState = StateUnavailable
)

// RecordKind distinguishes an account record from a group record.
type RecordKind uint8

const (
	RecordAccount RecordKind = iota + 1
	RecordGroup
	lastRecordKind = RecordGroup
)

func recordKindFor(op Opcode) RecordKind {
	switch op {
	case AdminGetAccount, AdminListAccounts, LookupGetAccount:
		return RecordAccount
	case AdminGetGroup, AdminListGroups:
		return RecordGroup
	}
	return 0
}

// Request is one decoded administration or lookup request. Which fields an
// opcode may set is fixed; every other field is empty or zero.
type Request struct {
	Opcode Opcode
	// Enabled is set only by AdminSetEnabled.
	Enabled bool
	// Name is the account or group name the operation targets.
	Name []byte
	// Home is set only by AdminSetHome.
	Home []byte
	// UID is the Unix user number set by AdminSetUID.
	UID uint32
	// GID is the primary group's Unix GID set by AdminSetGID.
	GID uint32
	// Key is the Ed25519 public key (KeySize bytes) for AdminAddKey and
	// LookupAuthorizeKey.
	Key []byte
	// Fingerprint is the SHA-256 key digest (FingerprintSize bytes) for
	// AdminRemoveKey.
	Fingerprint []byte
	// IDs is the supplementary Unix GID list for AdminSetGroups or the member
	// Unix UID list for AdminSetMembers.
	IDs []uint32
}

// Reply is one decoded administration or lookup reply. List replies use
// EncodeListReply/DecodeListReply instead: their chunks are notifications
// until the final reply so the request stays pending.
type Reply struct {
	Opcode Opcode
	Result Result
	// State, Accounts, Groups and Keys answer AdminStatus only.
	State    State
	Accounts uint32
	Groups   uint32
	Keys     uint32
	// Records carries the one record a get opcode returns, and is empty for
	// every other opcode.
	Records []Record
}

// Record is the bounded account/group shape carried by get and list replies.
// It carries core identity (IDs, numeric numbers, name, home, enabled and key
// count) plus the database epoch, committed revision and the account's
// supplementary group Unix GIDs, so a lookup reply is one coherent committed
// principal view (M4 FIX §5). Public keys are managed through their own
// operations and are not serialized in the record. GID is the Unix GID: the
// primary group's GID for an account record, the group's GID for a group
// record. AccountID/UID are meaningful only for accounts; GroupID is the
// internal primary group ID for accounts and the internal group ID for groups.
type Record struct {
	Kind      RecordKind
	AccountID uint64
	GroupID   uint64
	UID       uint32
	GID       uint32
	Enabled   bool
	// KeyCount is the number of authorized public keys, up to MaxKeysPerAccount.
	KeyCount uint8
	// Epoch is the 128-bit database epoch and Revision the committed mutation
	// sequence (Store.Sequence). Together they identify one committed view and
	// apply to both account and group records.
	Epoch    [16]byte
	Revision uint64
	// Groups are the account's supplementary group Unix GIDs, sorted ascending;
	// empty for group records.
	Groups []uint32
	Name   []byte
	Home   []byte
}

// EncodeControlRequest builds a control request that opens an administration
// or lookup session, handing identityd the session endpoint it must serve.
func EncodeControlRequest(op Opcode, session handle.ID, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || session == 0 || !isControlOpcode(op) || out == nil {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindRequest, correlation, controlSize)
	out.Payload[0] = byte(op)
	out.HandleCount, out.Handles[0] = 1, session
	return true
}

func DecodeControlRequest(in *ipc.Message) (Opcode, handle.ID, uint64, bool) {
	if in == nil || in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindRequest ||
		in.Correlation == 0 || in.PayloadSize != controlSize || in.HandleCount != 1 || in.Handles[0] == 0 ||
		!zero(in.Payload[1:controlSize]) {
		return 0, 0, 0, false
	}
	op := Opcode(in.Payload[0])
	if !isControlOpcode(op) {
		return 0, 0, 0, false
	}
	return op, in.Handles[0], in.Correlation, true
}

func EncodeControlReply(op Opcode, result Result, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !isControlOpcode(op) || result > lastResult || out == nil {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindReply, correlation, controlSize)
	out.Payload[0], out.Payload[1] = byte(op), byte(result)
	return true
}

func DecodeControlReply(in *ipc.Message, op Opcode, correlation uint64) (Result, bool) {
	if in == nil || in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply ||
		correlation == 0 || in.Correlation != correlation || in.PayloadSize != controlSize || in.HandleCount != 0 ||
		in.Payload[0] != byte(op) || Result(in.Payload[1]) > lastResult || !zero(in.Payload[2:controlSize]) {
		return 0, false
	}
	return Result(in.Payload[1]), true
}

// EncodeRequest encodes one administration or lookup request. It fails on a
// zero correlation, an unknown opcode, an oversized field, or a payload that
// would not fit the inline IPC limit.
func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || r == nil || out == nil || !validRequest(r) {
		return false
	}
	size := requestHeader + len(r.Name) + len(r.Home) + len(r.Key) + len(r.Fingerprint) + 4*len(r.IDs)
	if size > ipc.MaxPayload {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindRequest, correlation, uint16(size))
	payload := out.Payload[:size]
	payload[requestOpcode] = byte(r.Opcode)
	if r.Enabled {
		payload[requestFlags] = flagEnabled
	}
	payload[requestNameLen] = byte(len(r.Name))
	payload[requestHomeLen] = byte(len(r.Home))
	payload[requestIDCount] = byte(len(r.IDs))
	payload[requestKeyLen] = byte(len(r.Key))
	payload[requestFPLen] = byte(len(r.Fingerprint))
	binary.LittleEndian.PutUint32(payload[requestUID:], r.UID)
	binary.LittleEndian.PutUint32(payload[requestGID:], r.GID)
	offset := requestHeader
	copy(payload[offset:], r.Name)
	offset += len(r.Name)
	copy(payload[offset:], r.Home)
	offset += len(r.Home)
	copy(payload[offset:], r.Key)
	offset += len(r.Key)
	copy(payload[offset:], r.Fingerprint)
	offset += len(r.Fingerprint)
	for _, id := range r.IDs {
		binary.LittleEndian.PutUint32(payload[offset:], id)
		offset += 4
	}
	return true
}

// DecodeRequest validates a request and fills out, untouched on failure. It
// returns the correlation so the caller can attribute even a failed decode.
func DecodeRequest(in *ipc.Message, out *Request) (correlation uint64, ok bool) {
	if in == nil || out == nil || !requestEnvelope(in) || in.HandleCount != 0 || in.PayloadSize < requestHeader {
		return 0, false
	}
	payload := in.Payload[:in.PayloadSize]
	if payload[requestFlags]&^flagEnabled != 0 || payload[7] != 0 || !zero(payload[16:requestHeader]) {
		return 0, false
	}
	nameLen, homeLen := int(payload[requestNameLen]), int(payload[requestHomeLen])
	idCount, keyLen, fpLen := int(payload[requestIDCount]), int(payload[requestKeyLen]), int(payload[requestFPLen])
	if nameLen > identity.MaxName || homeLen > identity.MaxHome || idCount > identity.MaxAccounts ||
		keyLen > maxPackedKeyLen || fpLen > identity.FingerprintSize ||
		requestHeader+nameLen+homeLen+keyLen+fpLen+4*idCount != int(in.PayloadSize) {
		return 0, false
	}
	r := Request{
		Opcode:  Opcode(payload[requestOpcode]),
		Enabled: payload[requestFlags]&flagEnabled != 0,
		UID:     binary.LittleEndian.Uint32(payload[requestUID:]),
		GID:     binary.LittleEndian.Uint32(payload[requestGID:]),
	}
	offset := requestHeader
	r.Name = sliceOf(payload, offset, nameLen)
	offset += nameLen
	r.Home = sliceOf(payload, offset, homeLen)
	offset += homeLen
	r.Key = sliceOf(payload, offset, keyLen)
	offset += keyLen
	r.Fingerprint = sliceOf(payload, offset, fpLen)
	offset += fpLen
	if idCount > 0 {
		r.IDs = make([]uint32, idCount)
		for index := range r.IDs {
			r.IDs[index] = binary.LittleEndian.Uint32(payload[offset:])
			offset += 4
		}
	}
	if !validRequest(&r) {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

// RequestOpcode identifies a malformed data request the service can still
// answer: a GIDNTY1 request with a nonzero correlation and a known data
// opcode byte, whatever its size, other bytes or attachments.
func RequestOpcode(in *ipc.Message) (Opcode, uint64, bool) {
	if !requestEnvelope(in) || in.PayloadSize == 0 {
		return 0, 0, false
	}
	opcode := Opcode(in.Payload[requestOpcode])
	if !isDataOpcode(opcode) {
		return 0, 0, false
	}
	return opcode, in.Correlation, true
}

func requestEnvelope(in *ipc.Message) bool {
	return in != nil && in.ProtocolID == ProtocolID && in.Version == Version && in.Kind == ipc.KindRequest &&
		in.Correlation != 0 && in.PayloadSize <= ipc.MaxPayload
}

func validName(name []byte) bool { return len(name) >= 1 && len(name) <= identity.MaxName }
func validHome(home []byte) bool { return len(home) <= identity.MaxHome }
func validKey(key []byte) bool   { return len(key) == identity.KeySize }
func validFP(fp []byte) bool     { return len(fp) == identity.FingerprintSize }

func validRequest(r *Request) bool {
	if !isDataOpcode(r.Opcode) {
		return false
	}
	scalar := r.UID == 0 && r.GID == 0
	noData := len(r.Home) == 0 && len(r.Key) == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	switch r.Opcode {
	case AdminStatus, AdminProvision, AdminReset, AdminListAccounts, AdminListGroups:
		return !r.Enabled && len(r.Name) == 0 && scalar && noData
	case AdminGetAccount, AdminGetGroup, AdminAddAccount, AdminDeleteAccount, AdminAddGroup, AdminDeleteGroup, LookupGetAccount:
		return !r.Enabled && validName(r.Name) && scalar && noData
	case AdminSetEnabled:
		return validName(r.Name) && scalar && noData
	case AdminSetUID:
		return !r.Enabled && validName(r.Name) && r.GID == 0 && noData
	case AdminSetGID:
		return !r.Enabled && validName(r.Name) && r.UID == 0 && noData
	case AdminSetHome:
		return !r.Enabled && validName(r.Name) && validHome(r.Home) && scalar && len(r.Key) == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	case AdminSetGroups:
		return !r.Enabled && validName(r.Name) && scalar && len(r.Home) == 0 && len(r.Key) == 0 &&
			len(r.Fingerprint) == 0 && len(r.IDs) <= identity.MaxSupplementaryGroups
	case AdminAddKey:
		return !r.Enabled && validName(r.Name) && validKey(r.Key) && scalar && len(r.Home) == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	case AdminRemoveKey:
		return !r.Enabled && validName(r.Name) && validFP(r.Fingerprint) && scalar && len(r.Home) == 0 && len(r.Key) == 0 && len(r.IDs) == 0
	case AdminSetMembers:
		return !r.Enabled && validName(r.Name) && scalar && len(r.Home) == 0 && len(r.Key) == 0 &&
			len(r.Fingerprint) == 0 && len(r.IDs) <= identity.MaxAccounts
	case AdminSetGroupGID:
		return !r.Enabled && validName(r.Name) && r.UID == 0 && noData
	case AdminImportLegacy:
		_, keysOK := ParseImportKeys(r.Key)
		return !r.Enabled && validName(r.Name) && validHome(r.Home) && keysOK &&
			r.UID == 0 && r.GID == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	case LookupAuthorizeKey:
		return !r.Enabled && validName(r.Name) && validKey(r.Key) && scalar && len(r.Home) == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	case LookupAuthorizeAndResolve:
		return !r.Enabled && validName(r.Name) && validKey(r.Key) && scalar && len(r.Home) == 0 && len(r.Fingerprint) == 0 && len(r.IDs) == 0
	}
	return false
}

// MaxImportKeys bounds the keys the atomic legacy import carries, sized to the
// fixed IPC payload budget: name (31) + home (64) + count (1) + keys (4 x 32).
const MaxImportKeys = 4

// maxPackedKeyLen bounds Request.Key on the wire: one count byte plus the
// packed legacy-import keys. Opcode-specific validation still enforces the
// exact single-key size for key operations.
const maxPackedKeyLen = 1 + MaxImportKeys*identity.KeySize

// ParseImportKeys decodes the packed [count][key...] form the atomic legacy
// import carries in Request.Key.
func ParseImportKeys(packed []byte) ([]identity.Key, bool) {
	if len(packed) == 0 || packed[0] > MaxImportKeys {
		return nil, false
	}
	count := int(packed[0])
	if len(packed) != 1+count*identity.KeySize {
		return nil, false
	}
	var keys []identity.Key
	for i := 0; i < count; i++ {
		var key identity.Key
		copy(key[:], packed[1+i*identity.KeySize:])
		keys = append(keys, key)
	}
	return keys, true
}

// EncodeImportKeys packs validated public keys for Request.Key.
func EncodeImportKeys(keys [][]byte) []byte {
	if len(keys) > MaxImportKeys {
		return nil
	}
	out := make([]byte, 1, 1+len(keys)*identity.KeySize)
	out[0] = byte(len(keys))
	for _, key := range keys {
		out = append(out, key...)
	}
	return out
}

// EncodeReply encodes one administration or lookup reply, other than a list
// chunk. get opcodes carry one record on success; every other opcode carries
// no records.
func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || r == nil || out == nil || isListOpcode(r.Opcode) || !validReply(r) {
		return false
	}
	size := replyHeader
	for index := range r.Records {
		if !validRecord(&r.Records[index]) {
			return false
		}
		size += recordSize(&r.Records[index])
	}
	if size > ipc.MaxPayload {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindReply, correlation, uint16(size))
	payload := out.Payload[:size]
	payload[replyOpcode] = byte(r.Opcode)
	payload[replyResult] = byte(r.Result)
	payload[replyState] = byte(r.State)
	payload[replyCount] = byte(len(r.Records))
	binary.LittleEndian.PutUint64(payload[replyCorr:], correlation)
	binary.LittleEndian.PutUint32(payload[replyAccounts:], r.Accounts)
	binary.LittleEndian.PutUint32(payload[replyGroups:], r.Groups)
	binary.LittleEndian.PutUint32(payload[replyKeys:], r.Keys)
	offset := replyHeader
	for index := range r.Records {
		encodeRecord(payload[offset:], &r.Records[index])
		offset += recordSize(&r.Records[index])
	}
	return true
}

// DecodeReply accepts only the reply to the request the caller sent: the given
// correlation and opcode, with no list chunks. out is untouched on failure.
func DecodeReply(in *ipc.Message, correlation uint64, opcode Opcode, out *Reply) bool {
	if in == nil || out == nil || in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply ||
		correlation == 0 || in.Correlation != correlation || in.HandleCount != 0 ||
		in.PayloadSize < replyHeader || in.PayloadSize > ipc.MaxPayload ||
		Opcode(in.Payload[replyOpcode]) != opcode || in.Payload[replyMore] != 0 ||
		binary.LittleEndian.Uint64(in.Payload[replySequence:]) != 0 ||
		binary.LittleEndian.Uint64(in.Payload[replyCorr:]) != correlation ||
		!zero(in.Payload[5:8]) || !zero(in.Payload[36:replyHeader]) {
		return false
	}
	count := int(in.Payload[replyCount])
	if count > 1 {
		return false
	}
	r := Reply{
		Opcode:   opcode,
		Result:   Result(in.Payload[replyResult]),
		State:    State(in.Payload[replyState]),
		Accounts: binary.LittleEndian.Uint32(in.Payload[replyAccounts:]),
		Groups:   binary.LittleEndian.Uint32(in.Payload[replyGroups:]),
		Keys:     binary.LittleEndian.Uint32(in.Payload[replyKeys:]),
	}
	if count == 1 {
		r.Records = make([]Record, 1)
	}
	offset := replyHeader
	for index := 0; index < count; index++ {
		record, size, ok := decodeRecord(in.Payload[offset:])
		if !ok {
			return false
		}
		r.Records[index] = record
		offset += size
	}
	if offset != int(in.PayloadSize) || !validReply(&r) {
		return false
	}
	*out = r
	return true
}

func validReply(r *Reply) bool {
	if !isDataOpcode(r.Opcode) || r.Result > lastResult || r.State > lastState {
		return false
	}
	counts := r.Accounts == 0 && r.Groups == 0 && r.Keys == 0
	switch r.Opcode {
	case AdminStatus:
		if r.Result == ResultOK {
			return r.Accounts <= identity.MaxAccounts && r.Groups <= identity.MaxGroups && r.Keys <= identity.MaxTotalKeys && len(r.Records) == 0
		}
		return counts && len(r.Records) == 0
	case AdminGetAccount, AdminGetGroup, LookupGetAccount, LookupAuthorizeAndResolve:
		if r.Result == ResultOK {
			return counts && len(r.Records) == 1 && validRecord(&r.Records[0]) && r.Records[0].Kind == recordKindFor(r.Opcode)
		}
		return counts && len(r.Records) == 0
	default:
		return counts && len(r.Records) == 0
	}
}

// EncodeListReply encodes one chunk of a list reply. Non-final chunks are
// notifications so the IPC request stays pending until the final reply; the
// request correlation is carried in the payload for every chunk.
func EncodeListReply(op Opcode, result Result, sequence uint64, more bool, records []Record, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || out == nil || !isListOpcode(op) || result > lastResult {
		return false
	}
	if result != ResultOK && (more || sequence != 0 || len(records) != 0) {
		return false
	}
	max := identity.MaxAccounts
	if op == AdminListGroups {
		max = identity.MaxGroups
	}
	if len(records) > max {
		return false
	}
	size := replyHeader
	for index := range records {
		if !validRecord(&records[index]) || records[index].Kind != recordKindFor(op) {
			return false
		}
		size += recordSize(&records[index])
	}
	if size > ipc.MaxPayload {
		return false
	}
	kind, envelope := ipc.KindReply, correlation
	if more {
		kind, envelope = ipc.KindNotification, 0
	}
	prepare(out, ProtocolID, Version, kind, envelope, uint16(size))
	payload := out.Payload[:size]
	payload[replyOpcode] = byte(op)
	payload[replyResult] = byte(result)
	if more {
		payload[replyMore] = flagMore
	}
	payload[replyCount] = byte(len(records))
	binary.LittleEndian.PutUint64(payload[replyCorr:], correlation)
	binary.LittleEndian.PutUint64(payload[replySequence:], sequence)
	offset := replyHeader
	for index := range records {
		encodeRecord(payload[offset:], &records[index])
		offset += recordSize(&records[index])
	}
	return true
}

// DecodeListReply accepts one chunk of a list reply for the given correlation
// and opcode. It returns the result, sequence index, More flag and records.
func DecodeListReply(in *ipc.Message, correlation uint64, opcode Opcode) (Result, uint64, bool, []Record, bool) {
	fail := func() (Result, uint64, bool, []Record, bool) { return 0, 0, false, nil, false }
	if in == nil || in.ProtocolID != ProtocolID || in.Version != Version || correlation == 0 || !isListOpcode(opcode) ||
		in.HandleCount != 0 || in.PayloadSize < replyHeader || in.PayloadSize > ipc.MaxPayload ||
		Opcode(in.Payload[replyOpcode]) != opcode || binary.LittleEndian.Uint64(in.Payload[replyCorr:]) != correlation ||
		in.Payload[replyMore] > flagMore || !zero(in.Payload[5:8]) || !zero(in.Payload[36:replyHeader]) {
		return fail()
	}
	more := in.Payload[replyMore] == flagMore
	if more && (in.Kind != ipc.KindNotification || in.Correlation != 0) ||
		!more && (in.Kind != ipc.KindReply || in.Correlation != correlation) {
		return fail()
	}
	result := Result(in.Payload[replyResult])
	sequence := binary.LittleEndian.Uint64(in.Payload[replySequence:])
	if in.Payload[replyState] != 0 || binary.LittleEndian.Uint32(in.Payload[replyAccounts:]) != 0 ||
		binary.LittleEndian.Uint32(in.Payload[replyGroups:]) != 0 || binary.LittleEndian.Uint32(in.Payload[replyKeys:]) != 0 {
		return fail()
	}
	count := int(in.Payload[replyCount])
	if result != ResultOK {
		if more || sequence != 0 || count != 0 {
			return fail()
		}
		return result, sequence, false, nil, true
	}
	max := identity.MaxAccounts
	if opcode == AdminListGroups {
		max = identity.MaxGroups
	}
	if count > max {
		return fail()
	}
	var records []Record
	if count > 0 {
		records = make([]Record, count)
	}
	offset := replyHeader
	for index := 0; index < count; index++ {
		record, size, ok := decodeRecord(in.Payload[offset:])
		if !ok || record.Kind != recordKindFor(opcode) {
			return fail()
		}
		records[index] = record
		offset += size
	}
	if offset != int(in.PayloadSize) {
		return fail()
	}
	return result, sequence, more, records, true
}

func validRecord(r *Record) bool {
	if r == nil || r.Kind < RecordAccount || r.Kind > lastRecordKind || !validName(r.Name) || !validHome(r.Home) ||
		r.KeyCount > identity.MaxKeysPerAccount || len(r.Groups) > identity.MaxSupplementaryGroups {
		return false
	}
	for index := 1; index < len(r.Groups); index++ {
		if r.Groups[index-1] >= r.Groups[index] {
			return false
		}
	}
	switch r.Kind {
	case RecordAccount:
		return true
	case RecordGroup:
		return r.AccountID == 0 && r.UID == 0 && !r.Enabled && len(r.Home) == 0 && r.KeyCount == 0 && len(r.Groups) == 0
	}
	return false
}

func recordSize(r *Record) int {
	return recordFixed + len(r.Name) + len(r.Home) + 4*len(r.Groups)
}

func encodeRecord(payload []byte, r *Record) bool {
	if !validRecord(r) || recordSize(r) > len(payload) {
		return false
	}
	payload[recordKind] = byte(r.Kind)
	if r.Enabled {
		payload[recordEnabled] = 1
	}
	payload[recordNameLen] = byte(len(r.Name))
	payload[recordHomeLen] = byte(len(r.Home))
	payload[recordKeyCount] = r.KeyCount
	payload[recordGroupLen] = byte(len(r.Groups))
	binary.LittleEndian.PutUint32(payload[recordUID:], r.UID)
	binary.LittleEndian.PutUint32(payload[recordGID:], r.GID)
	binary.LittleEndian.PutUint64(payload[recordAccount:], r.AccountID)
	binary.LittleEndian.PutUint64(payload[recordGroup:], r.GroupID)
	copy(payload[recordEpoch:recordEpoch+16], r.Epoch[:])
	binary.LittleEndian.PutUint64(payload[recordRevision:], r.Revision)
	offset := recordFixed
	copy(payload[offset:], r.Name)
	offset += len(r.Name)
	copy(payload[offset:], r.Home)
	offset += len(r.Home)
	for _, gid := range r.Groups {
		binary.LittleEndian.PutUint32(payload[offset:], gid)
		offset += 4
	}
	return true
}

func decodeRecord(payload []byte) (Record, int, bool) {
	var r Record
	if len(payload) < recordFixed || !zero(payload[6:8]) {
		return r, 0, false
	}
	kind := RecordKind(payload[recordKind])
	if kind != RecordAccount && kind != RecordGroup || payload[recordEnabled] > 1 {
		return r, 0, false
	}
	nameLen, homeLen := int(payload[recordNameLen]), int(payload[recordHomeLen])
	keyCount := payload[recordKeyCount]
	groupLen := int(payload[recordGroupLen])
	if nameLen > identity.MaxName || homeLen > identity.MaxHome || keyCount > identity.MaxKeysPerAccount ||
		groupLen > identity.MaxSupplementaryGroups ||
		recordFixed+nameLen+homeLen+4*groupLen > len(payload) {
		return r, 0, false
	}
	r = Record{
		Kind:      kind,
		AccountID: binary.LittleEndian.Uint64(payload[recordAccount:]),
		GroupID:   binary.LittleEndian.Uint64(payload[recordGroup:]),
		UID:       binary.LittleEndian.Uint32(payload[recordUID:]),
		GID:       binary.LittleEndian.Uint32(payload[recordGID:]),
		Enabled:   payload[recordEnabled] == 1,
		KeyCount:  keyCount,
		Revision:  binary.LittleEndian.Uint64(payload[recordRevision:]),
		Name:      sliceOf(payload, recordFixed, nameLen),
		Home:      sliceOf(payload, recordFixed+nameLen, homeLen),
	}
	copy(r.Epoch[:], payload[recordEpoch:recordEpoch+16])
	if groupLen > 0 {
		r.Groups = make([]uint32, groupLen)
		offset := recordFixed + nameLen + homeLen
		for index := range r.Groups {
			r.Groups[index] = binary.LittleEndian.Uint32(payload[offset:])
			offset += 4
		}
	}
	if !validRecord(&r) {
		return Record{}, 0, false
	}
	return r, recordFixed + nameLen + homeLen + 4*groupLen, true
}

// prepare fills out field by field: a composite literal of an ipc.Message
// exceeds TinyGo's stack allocation limit and would leak heap on every call.
// The whole payload and handles are cleared so stale bytes never reach the
// peer.
func prepare(out *ipc.Message, protocol uint64, version uint16, kind ipc.Kind, correlation uint64, size uint16) {
	for index := range out.Payload {
		out.Payload[index] = 0
	}
	for index := range out.Handles {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = protocol, version, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, size, 0
}

func zero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

// sliceOf returns a nil slice for a zero length so a decoded value re-encodes
// byte-for-byte and compares equal to the struct it was encoded from.
func sliceOf(payload []byte, offset, length int) []byte {
	if length == 0 {
		return nil
	}
	return payload[offset : offset+length]
}
