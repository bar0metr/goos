package identityproto

import (
	"reflect"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/identity"
)

func testName() []byte { return []byte("alice") }
func testHome() []byte { return []byte("/home/alice") }
func testKey() []byte {
	key := make([]byte, identity.KeySize)
	for index := range key {
		key[index] = byte(index + 1)
	}
	return key
}
func testFP() []byte {
	fp := make([]byte, identity.FingerprintSize)
	for index := range fp {
		fp[index] = byte(0x80 + index)
	}
	return fp
}
func testEpoch() [16]byte {
	var epoch [16]byte
	for index := range epoch {
		epoch[index] = byte(index + 1)
	}
	return epoch
}

func requests() []Request {
	name, home, key, fp := testName(), testHome(), testKey(), testFP()
	return []Request{
		{Opcode: AdminStatus},
		{Opcode: AdminProvision},
		{Opcode: AdminReset},
		{Opcode: AdminListAccounts},
		{Opcode: AdminListGroups},
		{Opcode: AdminGetAccount, Name: name},
		{Opcode: AdminGetGroup, Name: []byte("wheel")},
		{Opcode: AdminAddAccount, Name: name},
		{Opcode: AdminDeleteAccount, Name: name},
		{Opcode: AdminSetEnabled, Name: name, Enabled: true},
		{Opcode: AdminSetEnabled, Name: name},
		{Opcode: AdminSetUID, Name: name, UID: 1000},
		{Opcode: AdminSetGID, Name: name, GID: 1000},
		{Opcode: AdminSetHome, Name: name, Home: home},
		{Opcode: AdminSetGroups, Name: name, IDs: []uint32{1000, 1001}},
		{Opcode: AdminAddKey, Name: name, Key: key},
		{Opcode: AdminRemoveKey, Name: name, Fingerprint: fp},
		{Opcode: AdminAddGroup, Name: []byte("wheel")},
		{Opcode: AdminDeleteGroup, Name: []byte("wheel")},
		{Opcode: AdminSetMembers, Name: []byte("wheel"), IDs: []uint32{1000, 1001, 1002}},
		{Opcode: LookupAuthorizeKey, Name: name, Key: key},
		{Opcode: LookupGetAccount, Name: name},
	}
}

func replies() []Reply {
	account := Record{Kind: RecordAccount, AccountID: 1, GroupID: 2, UID: 1000, GID: 1000, Enabled: true, KeyCount: 3, Epoch: testEpoch(), Revision: 42, Groups: []uint32{1001, 1002}, Name: testName(), Home: testHome()}
	group := Record{Kind: RecordGroup, GroupID: 2, GID: 1000, Epoch: testEpoch(), Revision: 42, Name: []byte("wheel")}
	return []Reply{
		{Opcode: AdminStatus, Result: ResultOK, State: StateAvailable, Accounts: 3, Groups: 2, Keys: 5},
		{Opcode: AdminStatus, Result: ResultOK, State: StateUnprovisioned},
		{Opcode: AdminStatus, Result: ResultUnavailable, State: StateUnavailable},
		{Opcode: AdminStatus, Result: ResultInvalid},
		{Opcode: AdminProvision},
		{Opcode: AdminReset},
		{Opcode: AdminGetAccount, Result: ResultOK, Records: []Record{account}},
		{Opcode: AdminGetAccount, Result: ResultNotFound},
		{Opcode: AdminGetGroup, Result: ResultOK, Records: []Record{group}},
		{Opcode: AdminAddAccount, Result: ResultExists},
		{Opcode: AdminDeleteAccount},
		{Opcode: AdminSetEnabled},
		{Opcode: AdminSetUID, Result: ResultInvalid},
		{Opcode: AdminSetGID},
		{Opcode: AdminSetHome},
		{Opcode: AdminSetGroups},
		{Opcode: AdminAddKey, Result: ResultExhausted},
		{Opcode: AdminRemoveKey},
		{Opcode: AdminAddGroup, Result: ResultExists},
		{Opcode: AdminDeleteGroup, Result: ResultConflict},
		{Opcode: AdminSetMembers},
		{Opcode: LookupAuthorizeKey, Result: ResultOK},
		{Opcode: LookupAuthorizeKey, Result: ResultDenied},
		{Opcode: LookupGetAccount, Result: ResultOK, Records: []Record{account}},
	}
}

func listRecords() []Record {
	return []Record{
		{Kind: RecordAccount, AccountID: 1, GroupID: 2, UID: 1000, GID: 1000, Enabled: true, KeyCount: 1, Epoch: testEpoch(), Revision: 7, Groups: []uint32{1001, 1002}, Name: testName(), Home: testHome()},
		{Kind: RecordAccount, AccountID: 3, GroupID: 2, UID: 1001, Name: []byte("bob")},
	}
}

func TestControlRoundTrip(t *testing.T) {
	for _, op := range []Opcode{OpAddAdmin, OpAddLookup} {
		var wire ipc.Message
		if !EncodeControlRequest(op, 9, 17, &wire) {
			t.Fatalf("encode %d", op)
		}
		decoded, session, correlation, ok := DecodeControlRequest(&wire)
		if !ok || decoded != op || session != 9 || correlation != 17 {
			t.Fatalf("decode %d: %d %d %d %v", op, decoded, session, correlation, ok)
		}
		for _, result := range []Result{ResultOK, ResultDenied} {
			if !EncodeControlReply(op, result, 17, &wire) {
				t.Fatalf("encode reply %d", op)
			}
			got, ok := DecodeControlReply(&wire, op, 17)
			if !ok || got != result {
				t.Fatalf("decode reply %d %d: %d %v", op, result, got, ok)
			}
		}
	}
	if EncodeControlRequest(OpAddAdmin, 0, 1, &ipc.Message{}) || EncodeControlRequest(OpAddAdmin, 9, 0, &ipc.Message{}) {
		t.Fatal("encoded control request without session or correlation")
	}
	if EncodeControlRequest(AdminStatus, 9, 1, &ipc.Message{}) {
		t.Fatal("encoded a data opcode as control")
	}
}

func TestControlRejects(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version", func(m *ipc.Message) { m.Version = 0 }},
		{"reply kind", func(m *ipc.Message) { m.Kind = ipc.KindReply }},
		{"no correlation", func(m *ipc.Message) { m.Correlation = 0 }},
		{"short", func(m *ipc.Message) { m.PayloadSize = controlSize - 1 }},
		{"long", func(m *ipc.Message) { m.PayloadSize = controlSize + 1 }},
		{"no handle", func(m *ipc.Message) { m.HandleCount = 0 }},
		{"two handles", func(m *ipc.Message) { m.HandleCount, m.Handles[1] = 2, 8 }},
		{"unknown opcode", func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) }},
		{"reserved", func(m *ipc.Message) { m.Payload[3] = 1 }},
	}
	var wire ipc.Message
	EncodeControlRequest(OpAddAdmin, 9, 7, &wire)
	for _, mutation := range mutations {
		message := wire
		mutation.mutate(&message)
		if _, _, _, ok := DecodeControlRequest(&message); ok {
			t.Fatalf("accepted %s", mutation.name)
		}
	}
	EncodeControlReply(OpAddAdmin, ResultOK, 7, &wire)
	for _, mutation := range []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"wrong opcode", func(m *ipc.Message) { m.Payload[0] = byte(OpAddLookup) }},
		{"result", func(m *ipc.Message) { m.Payload[1] = byte(lastResult + 1) }},
		{"correlation", func(m *ipc.Message) { m.Correlation++ }},
		{"reserved", func(m *ipc.Message) { m.Payload[5] = 1 }},
	} {
		message := wire
		mutation.mutate(&message)
		if _, ok := DecodeControlReply(&message, OpAddAdmin, 7); ok {
			t.Fatalf("accepted control reply %s", mutation.name)
		}
	}
}

func TestRequestRoundTrip(t *testing.T) {
	for _, request := range requests() {
		var wire ipc.Message
		wire.Payload[100] = 1
		if !EncodeRequest(&request, 17, &wire) || wire.Payload[100] != 0 || wire.Kind != ipc.KindRequest {
			t.Fatalf("encode %d", request.Opcode)
		}
		var decoded Request
		correlation, ok := DecodeRequest(&wire, &decoded)
		if !ok || correlation != 17 || !reflect.DeepEqual(decoded, request) {
			t.Fatalf("decode %d: %+v", request.Opcode, decoded)
		}
		opcode, correlation, ok := RequestOpcode(&wire)
		if !ok || opcode != request.Opcode || correlation != 17 {
			t.Fatalf("request opcode %d", request.Opcode)
		}
	}
}

func TestRequestInvalid(t *testing.T) {
	name, key, fp := testName(), testKey(), testFP()
	invalid := []Request{
		{},
		{Opcode: lastOpcode + 1},
		{Opcode: OpAddAdmin},
		{Opcode: OpAddLookup},
		{Opcode: AdminStatus, Name: name},
		{Opcode: AdminProvision, UID: 1},
		{Opcode: AdminListAccounts, Enabled: true},
		{Opcode: AdminGetAccount},
		{Opcode: AdminGetAccount, Name: make([]byte, identity.MaxName+1)},
		{Opcode: AdminAddAccount, Name: name, GID: 1},
		{Opcode: AdminDeleteAccount, Name: name, Enabled: true},
		{Opcode: AdminSetEnabled, UID: 1},
		{Opcode: AdminSetUID, Name: name, GID: 1},
		{Opcode: AdminSetUID, Name: name, UID: 1, Home: []byte("x")},
		{Opcode: AdminSetGID, Name: name, UID: 1},
		{Opcode: AdminSetHome, Name: name, Home: make([]byte, identity.MaxHome+1)},
		{Opcode: AdminSetHome, UID: 1, Home: []byte("x")},
		{Opcode: AdminSetGroups, Name: name, IDs: make([]uint32, identity.MaxSupplementaryGroups+1)},
		{Opcode: AdminSetGroups, Name: name, Key: key},
		{Opcode: AdminAddKey, Name: name},
		{Opcode: AdminAddKey, Name: name, Key: make([]byte, identity.KeySize-1)},
		{Opcode: AdminAddKey, Name: name, Key: make([]byte, identity.KeySize+1)},
		{Opcode: AdminRemoveKey, Name: name, Fingerprint: make([]byte, identity.FingerprintSize-1)},
		{Opcode: AdminAddGroup, Name: name, Key: key},
		{Opcode: AdminSetMembers, Name: name, IDs: make([]uint32, identity.MaxAccounts+1)},
		{Opcode: LookupAuthorizeKey, Name: name},
		{Opcode: LookupAuthorizeKey, Name: name, Key: make([]byte, identity.KeySize+1)},
		{Opcode: LookupGetAccount, Name: name, Fingerprint: fp},
	}
	for index, request := range invalid {
		var wire ipc.Message
		if EncodeRequest(&request, 1, &wire) {
			t.Fatalf("encoded invalid request %d: %+v", index, request)
		}
	}
	valid := requests()[0]
	var wire ipc.Message
	if EncodeRequest(&valid, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
}

func TestRequestRejects(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version zero", func(m *ipc.Message) { m.Version = 0 }},
		{"old version", func(m *ipc.Message) { m.Version = 1 }},
		{"reply kind", func(m *ipc.Message) { m.Kind = ipc.KindReply }},
		{"notification", func(m *ipc.Message) { m.Kind = ipc.KindNotification }},
		{"no correlation", func(m *ipc.Message) { m.Correlation = 0 }},
		{"short", func(m *ipc.Message) { m.PayloadSize-- }},
		{"long", func(m *ipc.Message) { m.PayloadSize = ipc.MaxPayload }},
		{"attachment", func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 9 }},
		{"zero opcode", func(m *ipc.Message) { m.Payload[0] = 0 }},
		{"unknown opcode", func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) }},
		{"control opcode", func(m *ipc.Message) { m.Payload[0] = byte(OpAddAdmin) }},
		{"reserved flag", func(m *ipc.Message) { m.Payload[1] |= 0x80 }},
		{"reserved byte", func(m *ipc.Message) { m.Payload[7] = 1 }},
		{"reserved tail", func(m *ipc.Message) { m.Payload[20] = 1 }},
		{"name too long", func(m *ipc.Message) { m.Payload[2] = identity.MaxName + 1 }},
		{"key too long", func(m *ipc.Message) { m.Payload[5] = identity.KeySize + 1 }},
	}
	for _, request := range requests() {
		for _, mutation := range mutations {
			var wire ipc.Message
			EncodeRequest(&request, 5, &wire)
			mutation.mutate(&wire)
			var decoded Request
			if _, ok := DecodeRequest(&wire, &decoded); ok || !reflect.DeepEqual(decoded, Request{}) {
				t.Fatalf("%d: accepted %s", request.Opcode, mutation.name)
			}
		}
	}
}

// Flipping any request byte the opcode leaves unused must make it undecodable,
// and the opcode must stay attributable so the service can still answer.
func TestRequestUnusedBytes(t *testing.T) {
	for _, request := range requests() {
		unused := append([]int{7}, span(16, requestHeader)...)
		if request.Opcode != AdminSetEnabled {
			unused = append(unused, 1)
		}
		if request.Opcode != AdminSetGID {
			unused = append(unused, span(requestGID, requestGID+4)...)
		}
		if request.Opcode != AdminSetUID {
			unused = append(unused, span(requestUID, requestUID+4)...)
		}
		for _, index := range unused {
			var wire ipc.Message
			EncodeRequest(&request, 5, &wire)
			wire.Payload[index] = 0x80
			var decoded Request
			if _, ok := DecodeRequest(&wire, &decoded); ok {
				t.Fatalf("%d: accepted nonzero unused byte %d", request.Opcode, index)
			}
			if opcode, _, ok := RequestOpcode(&wire); !ok || opcode != request.Opcode {
				t.Fatalf("%d: byte %d made the request unattributable", request.Opcode, index)
			}
		}
	}
}

func TestRequestOpcodeAttribution(t *testing.T) {
	wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 3, PayloadSize: 200, HandleCount: 2}
	wire.Payload[0] = byte(AdminGetAccount)
	if opcode, correlation, ok := RequestOpcode(&wire); !ok || opcode != AdminGetAccount || correlation != 3 {
		t.Fatal("oversized request not attributable")
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.PayloadSize = 0 },
		func(m *ipc.Message) { m.Payload[0] = 0 },
		func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) },
		func(m *ipc.Message) { m.Payload[0] = byte(OpAddAdmin) },
		func(m *ipc.Message) { m.Kind = ipc.KindNotification },
		func(m *ipc.Message) { m.ProtocolID = 0 },
	} {
		bad := wire
		mutate(&bad)
		if _, _, ok := RequestOpcode(&bad); ok {
			t.Fatal("attributed a non-request")
		}
	}
}

func TestReplyRoundTrip(t *testing.T) {
	for index, reply := range replies() {
		var wire ipc.Message
		if !EncodeReply(&reply, 99, &wire) || wire.Kind != ipc.KindReply {
			t.Fatalf("reply %d: encode", index)
		}
		var decoded Reply
		if !DecodeReply(&wire, 99, reply.Opcode, &decoded) || !reflect.DeepEqual(decoded, reply) {
			t.Fatalf("reply %d: decoded %+v", index, decoded)
		}
	}
}

func TestReplyInvalid(t *testing.T) {
	account := Record{Kind: RecordAccount, AccountID: 1, GroupID: 2, UID: 1000, Name: testName()}
	invalid := []Reply{
		{Opcode: 0},
		{Opcode: lastOpcode + 1},
		{Opcode: OpAddAdmin},
		{Opcode: AdminStatus, Result: lastResult + 1},
		{Opcode: AdminStatus, State: lastState + 1},
		{Opcode: AdminStatus, Result: ResultOK, Accounts: identity.MaxAccounts + 1},
		{Opcode: AdminStatus, Result: ResultOK, Groups: identity.MaxGroups + 1},
		{Opcode: AdminStatus, Result: ResultOK, Keys: identity.MaxTotalKeys + 1},
		{Opcode: AdminStatus, Result: ResultInvalid, Accounts: 1},
		{Opcode: AdminStatus, Result: ResultOK, Records: []Record{account}},
		{Opcode: AdminGetAccount, Result: ResultOK},
		{Opcode: AdminGetAccount, Result: ResultOK, Records: []Record{account, account}},
		{Opcode: AdminGetAccount, Result: ResultNotFound, Records: []Record{account}},
		{Opcode: AdminGetGroup, Result: ResultOK, Records: []Record{account}},
		{Opcode: AdminProvision, Accounts: 1},
		{Opcode: AdminReset, Records: []Record{account}},
	}
	for index, reply := range invalid {
		var wire ipc.Message
		if EncodeReply(&reply, 1, &wire) {
			t.Fatalf("encoded invalid reply %d: %+v", index, reply)
		}
	}
}

func TestReplyRejects(t *testing.T) {
	reply := replies()[0]
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version zero", func(m *ipc.Message) { m.Version = 0 }},
		{"old version", func(m *ipc.Message) { m.Version = 1 }},
		{"request kind", func(m *ipc.Message) { m.Kind = ipc.KindRequest }},
		{"correlation", func(m *ipc.Message) { m.Correlation = 8 }},
		{"payload correlation", func(m *ipc.Message) { binaryPut(m, replyCorr, 8) }},
		{"short", func(m *ipc.Message) { m.PayloadSize-- }},
		{"long", func(m *ipc.Message) { m.PayloadSize = ipc.MaxPayload }},
		{"attachment", func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 2 }},
		{"opcode", func(m *ipc.Message) { m.Payload[0] = byte(AdminProvision) }},
		{"result", func(m *ipc.Message) { m.Payload[1] = byte(lastResult + 1) }},
		{"state", func(m *ipc.Message) { m.Payload[2] = byte(lastState + 1) }},
		{"more", func(m *ipc.Message) { m.Payload[3] = 1 }},
		{"sequence", func(m *ipc.Message) { binaryPut(m, replySequence, 1) }},
		{"reserved", func(m *ipc.Message) { m.Payload[5] = 1 }},
		{"reserved tail", func(m *ipc.Message) { m.Payload[36] = 1 }},
		{"count", func(m *ipc.Message) { m.Payload[4] = 1 }},
	}
	for _, mutation := range mutations {
		var wire ipc.Message
		EncodeReply(&reply, 7, &wire)
		mutation.mutate(&wire)
		var decoded Reply
		if DecodeReply(&wire, 7, reply.Opcode, &decoded) || !reflect.DeepEqual(decoded, Reply{}) {
			t.Fatalf("accepted %s", mutation.name)
		}
	}
	var wire ipc.Message
	EncodeReply(&reply, 7, &wire)
	var decoded Reply
	if DecodeReply(&wire, 7, AdminProvision, &decoded) || DecodeReply(&wire, 0, reply.Opcode, &decoded) {
		t.Fatal("accepted unexpected opcode or zero correlation")
	}
	if EncodeReply(&reply, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
}

// Flipping any byte of a get reply's record reserved bytes must fail decoding.
func TestReplyRecordReservedBytes(t *testing.T) {
	reply := replies()[6] // AdminGetAccount with a record
	var wire ipc.Message
	EncodeReply(&reply, 4, &wire)
	for _, index := range []int{replyHeader + 6, replyHeader + 7} {
		message := wire
		message.Payload[index] = 0x80
		var decoded Reply
		if DecodeReply(&message, 4, reply.Opcode, &decoded) {
			t.Fatalf("accepted nonzero record reserved byte %d", index)
		}
	}
}

// TestRecordEpochRevisionGroups verifies the epoch, revision and supplementary
// group fields round-trip exactly through both get and list codecs, including a
// group record carrying a nonzero epoch and revision.
func TestRecordEpochRevisionGroups(t *testing.T) {
	epoch := testEpoch()
	revision := uint64(0x0102030405060708)
	account := Record{Kind: RecordAccount, AccountID: 9, GroupID: 4, UID: 5000, GID: 5000, Enabled: true, KeyCount: 2, Epoch: epoch, Revision: revision, Groups: []uint32{10, 20, 30, 40}, Name: testName(), Home: testHome()}
	group := Record{Kind: RecordGroup, GroupID: 4, GID: 5000, Epoch: epoch, Revision: revision, Name: []byte("wheel")}

	reply := Reply{Opcode: AdminGetAccount, Result: ResultOK, State: StateAvailable, Records: []Record{account}}
	var wire ipc.Message
	if !EncodeReply(&reply, 55, &wire) {
		t.Fatal("encode account get reply")
	}
	var decoded Reply
	if !DecodeReply(&wire, 55, AdminGetAccount, &decoded) || !reflect.DeepEqual(decoded, reply) {
		t.Fatalf("account get round-trip: %+v", decoded)
	}

	greply := Reply{Opcode: AdminGetGroup, Result: ResultOK, State: StateAvailable, Records: []Record{group}}
	if !EncodeReply(&greply, 56, &wire) {
		t.Fatal("encode group get reply")
	}
	if !DecodeReply(&wire, 56, AdminGetGroup, &decoded) || !reflect.DeepEqual(decoded, greply) {
		t.Fatalf("group get round-trip: %+v", decoded)
	}

	if !EncodeListReply(AdminListAccounts, ResultOK, 2, false, []Record{account}, 57, &wire) {
		t.Fatal("encode account list")
	}
	result, seq, more, records, ok := DecodeListReply(&wire, 57, AdminListAccounts)
	if !ok || result != ResultOK || seq != 2 || more || !reflect.DeepEqual(records, []Record{account}) {
		t.Fatal("account list round-trip")
	}
}

// TestRecordFieldRejects corrupts the newly added record fields and verifies
// decoding fails: a supplementary group list longer than the bound, a tampered
// group count, and out-of-order or duplicate groups.
func TestRecordFieldRejects(t *testing.T) {
	account := Record{Kind: RecordAccount, AccountID: 1, GroupID: 2, UID: 1000, GID: 1000, Enabled: true, KeyCount: 1, Epoch: testEpoch(), Revision: 42, Groups: []uint32{10, 20, 30}, Name: testName(), Home: testHome()}
	reply := Reply{Opcode: AdminGetAccount, Result: ResultOK, State: StateAvailable, Records: []Record{account}}
	var wire ipc.Message
	if !EncodeReply(&reply, 7, &wire) {
		t.Fatal("encode")
	}
	groupStart := replyHeader + recordFixed + len(account.Name) + len(account.Home)
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"group count overflow", func(m *ipc.Message) { m.Payload[replyHeader+recordGroupLen] = identity.MaxSupplementaryGroups + 1 }},
		{"group count tamper", func(m *ipc.Message) { m.Payload[replyHeader+recordGroupLen] = 1 }},
		{"groups unsorted", func(m *ipc.Message) { m.Payload[groupStart+3] = 0xff }},
		{"groups duplicate", func(m *ipc.Message) { m.Payload[groupStart+4] = 10 }},
	}
	for _, mutation := range mutations {
		message := wire
		mutation.mutate(&message)
		var decoded Reply
		if DecodeReply(&message, 7, AdminGetAccount, &decoded) {
			t.Fatalf("accepted %s", mutation.name)
		}
	}
}

func TestListRoundTrip(t *testing.T) {
	records := listRecords()
	var wire ipc.Message
	if !EncodeListReply(AdminListAccounts, ResultOK, 0, true, records, 9, &wire) {
		t.Fatal("encode non-final")
	}
	if wire.Kind != ipc.KindNotification || wire.Correlation != 0 {
		t.Fatal("non-final chunk must be a notification")
	}
	result, seq, more, got, ok := DecodeListReply(&wire, 9, AdminListAccounts)
	if !ok || result != ResultOK || seq != 0 || !more || !reflect.DeepEqual(got, records) {
		t.Fatalf("decode non-final: %d %d %v %+v %v", result, seq, more, got, ok)
	}
	if !EncodeListReply(AdminListAccounts, ResultOK, 1, false, records, 9, &wire) {
		t.Fatal("encode final")
	}
	if wire.Kind != ipc.KindReply || wire.Correlation != 9 {
		t.Fatal("final chunk must be a reply")
	}
	result, seq, more, got, ok = DecodeListReply(&wire, 9, AdminListAccounts)
	if !ok || result != ResultOK || seq != 1 || more || !reflect.DeepEqual(got, records) {
		t.Fatalf("decode final: %d %d %v %+v %v", result, seq, more, got, ok)
	}
	// An empty final chunk terminates an empty list.
	if !EncodeListReply(AdminListGroups, ResultOK, 0, false, nil, 9, &wire) {
		t.Fatal("encode empty list")
	}
	result, seq, more, got, ok = DecodeListReply(&wire, 9, AdminListGroups)
	if !ok || result != ResultOK || seq != 0 || more || len(got) != 0 {
		t.Fatal("decode empty list")
	}
	// A non-OK result terminates the list with no records.
	if !EncodeListReply(AdminListAccounts, ResultUnavailable, 0, false, nil, 9, &wire) {
		t.Fatal("encode error list")
	}
	result, seq, more, got, ok = DecodeListReply(&wire, 9, AdminListAccounts)
	if !ok || result != ResultUnavailable || seq != 0 || more || len(got) != 0 {
		t.Fatal("decode error list")
	}
}

func TestListRejects(t *testing.T) {
	records := listRecords()
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version", func(m *ipc.Message) { m.Version = 0 }},
		{"wrong correlation", func(m *ipc.Message) { m.Correlation = 8 }},
		{"payload correlation", func(m *ipc.Message) { binaryPut(m, replyCorr, 8) }},
		{"wrong kind", func(m *ipc.Message) { m.Kind = ipc.KindRequest }},
		{"short", func(m *ipc.Message) { m.PayloadSize-- }},
		{"reserved", func(m *ipc.Message) { m.Payload[5] = 1 }},
		{"reserved tail", func(m *ipc.Message) { m.Payload[36] = 1 }},
		{"state", func(m *ipc.Message) { m.Payload[2] = 1 }},
		{"counts", func(m *ipc.Message) { binaryPut(m, replyAccounts, 1) }},
		{"record reserved", func(m *ipc.Message) { m.Payload[replyHeader+6] = 1 }},
	}
	var wire ipc.Message
	EncodeListReply(AdminListAccounts, ResultOK, 0, true, records, 9, &wire)
	for _, mutation := range mutations {
		message := wire
		mutation.mutate(&message)
		if _, _, _, _, ok := DecodeListReply(&message, 9, AdminListAccounts); ok {
			t.Fatalf("accepted %s", mutation.name)
		}
	}
	// Wrong opcode, and record of the wrong kind.
	EncodeListReply(AdminListAccounts, ResultOK, 0, false, records, 9, &wire)
	if _, _, _, _, ok := DecodeListReply(&wire, 9, AdminListGroups); ok {
		t.Fatal("accepted wrong list opcode")
	}
	groupRecords := []Record{{Kind: RecordGroup, GroupID: 2, GID: 1000, Name: []byte("wheel")}}
	if EncodeListReply(AdminListAccounts, ResultOK, 0, false, groupRecords, 9, &wire) {
		t.Fatal("encoded a group record in an account list")
	}
}

func TestListMultipartKeepsRequestPendingUntilFinalReply(t *testing.T) {
	channel := ipc.NewChannel()
	var request ipc.Message
	if !EncodeRequest(&Request{Opcode: AdminListAccounts}, 7, &request) || channel.Send(0, request, nil, nil, nil) != ipc.StatusOK {
		t.Fatal("request")
	}
	if _, status := channel.Receive(1); status != ipc.StatusOK {
		t.Fatal("receive request", status)
	}
	records := listRecords()
	for part := 0; part < 3; part++ {
		more := part < 2
		var message ipc.Message
		if !EncodeListReply(AdminListAccounts, ResultOK, uint64(part), more, records, 7, &message) {
			t.Fatal("encode part", part)
		}
		if status := channel.Send(1, message, nil, nil, nil); status != ipc.StatusOK {
			t.Fatalf("part %d rejected by IPC: %d", part, status)
		}
		wantPending := 1
		if !more {
			wantPending = 0
		}
		if channel.Pending(0) != wantPending {
			t.Fatal("request completed before final part", part)
		}
		incoming, status := channel.Receive(0)
		if status != ipc.StatusOK {
			t.Fatal("receive part", status)
		}
		result, seq, remaining, got, ok := DecodeListReply(&incoming, 7, AdminListAccounts)
		if !ok || result != ResultOK || seq != uint64(part) || remaining != more || !reflect.DeepEqual(got, records) {
			t.Fatal("decode part", part)
		}
	}
	if channel.Send(1, ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: 7}, nil, nil, nil) != ipc.StatusNotFound {
		t.Fatal("final reply did not complete request")
	}
}

func TestVersionRejects(t *testing.T) {
	request := requests()[0]
	var wire ipc.Message
	EncodeRequest(&request, 1, &wire)
	for _, version := range []uint16{0, 1, 3} {
		message := wire
		message.Version = version
		var decoded Request
		if _, ok := DecodeRequest(&message, &decoded); ok {
			t.Fatalf("request version %d accepted", version)
		}
	}
	reply := replies()[0]
	EncodeReply(&reply, 1, &wire)
	for _, version := range []uint16{0, 1, 3} {
		message := wire
		message.Version = version
		var decoded Reply
		if DecodeReply(&message, 1, reply.Opcode, &decoded) {
			t.Fatalf("reply version %d accepted", version)
		}
	}
	EncodeControlRequest(OpAddAdmin, 9, 1, &wire)
	for _, version := range []uint16{0, 1, 3} {
		message := wire
		message.Version = version
		if _, _, _, ok := DecodeControlRequest(&message); ok {
			t.Fatalf("control version %d accepted", version)
		}
	}
}

func TestBounds(t *testing.T) {
	name := testName()
	if EncodeRequest(&Request{Opcode: AdminGetAccount, Name: make([]byte, identity.MaxName+1)}, 1, &ipc.Message{}) {
		t.Fatal("encoded oversized name")
	}
	if EncodeRequest(&Request{Opcode: AdminSetHome, Name: name, Home: make([]byte, identity.MaxHome+1)}, 1, &ipc.Message{}) {
		t.Fatal("encoded oversized home")
	}
	if EncodeRequest(&Request{Opcode: AdminSetGroups, Name: name, IDs: make([]uint32, identity.MaxSupplementaryGroups+1)}, 1, &ipc.Message{}) {
		t.Fatal("encoded oversized supplementary groups")
	}
	if EncodeRequest(&Request{Opcode: AdminSetMembers, Name: name, IDs: make([]uint32, identity.MaxAccounts+1)}, 1, &ipc.Message{}) {
		t.Fatal("encoded oversized members")
	}
	if EncodeRequest(&Request{Opcode: AdminSetMembers, Name: name, IDs: make([]uint32, identity.MaxAccounts)}, 1, &ipc.Message{}) {
		t.Fatal("encoded a member list too large for the inline payload")
	}
	// The largest member list that still fits the inline payload round-trips.
	maxMembers := (ipc.MaxPayload - requestHeader - len(name)) / 4
	var wire ipc.Message
	if !EncodeRequest(&Request{Opcode: AdminSetMembers, Name: name, IDs: make([]uint32, maxMembers)}, 1, &wire) {
		t.Fatal("largest inline member list did not encode")
	}
	var decoded Request
	if _, ok := DecodeRequest(&wire, &decoded); !ok || len(decoded.IDs) != maxMembers {
		t.Fatal("largest inline member list did not round-trip")
	}

	// A record with more than MaxSupplementaryGroups supplementary groups is
	// rejected; a list whose records exceed the inline payload is rejected; and
	// the largest single record still round-trips through a get reply.
	tooMany := Record{Kind: RecordAccount, AccountID: 1, GroupID: 1, UID: 1000, GID: 1000, Name: name, Groups: make([]uint32, identity.MaxSupplementaryGroups+1)}
	if EncodeReply(&Reply{Opcode: AdminGetAccount, Result: ResultOK, State: StateAvailable, Records: []Record{tooMany}}, 1, &ipc.Message{}) {
		t.Fatal("encoded a record with too many supplementary groups")
	}
	big := Record{Kind: RecordAccount, AccountID: 1, GroupID: 1, UID: 1000, GID: 1000, Name: name, Home: make([]byte, identity.MaxHome), Groups: make([]uint32, identity.MaxSupplementaryGroups)}
	for index := range big.Groups {
		big.Groups[index] = uint32(1000 + index)
	}
	if EncodeListReply(AdminListAccounts, ResultOK, 0, false, []Record{big, big}, 1, &ipc.Message{}) {
		t.Fatal("encoded a list that exceeds the inline payload")
	}
	var recordWire ipc.Message
	if !EncodeReply(&Reply{Opcode: AdminGetAccount, Result: ResultOK, State: StateAvailable, Records: []Record{big}}, 1, &recordWire) {
		t.Fatal("largest inline record did not encode")
	}
	var recordDecoded Reply
	if !DecodeReply(&recordWire, 1, AdminGetAccount, &recordDecoded) || !reflect.DeepEqual(recordDecoded.Records, []Record{big}) {
		t.Fatal("largest inline record did not round-trip")
	}
}

func TestEncodeRequestClearsReusedMessage(t *testing.T) {
	var message ipc.Message
	for index := range message.Payload {
		message.Payload[index] = 0xab
	}
	message.HandleCount = 2
	message.Handles[0] = 7
	if !EncodeRequest(&Request{Opcode: AdminGetAccount, Name: testName()}, 9, &message) {
		t.Fatal("encode failed")
	}
	if message.HandleCount != 0 || message.Handles[0] != 0 {
		t.Fatal("stale handle attachments survived encoding")
	}
	for index := requestHeader + len(testName()); index < len(message.Payload); index++ {
		if message.Payload[index] != 0 {
			t.Fatalf("stale payload byte at %d", index)
		}
	}
}

func span(from, to int) []int {
	var out []int
	for index := from; index < to; index++ {
		out = append(out, index)
	}
	return out
}

func binaryPut(m *ipc.Message, offset int, value uint64) {
	for index := 0; index < 8; index++ {
		m.Payload[offset+index] = byte(value >> (8 * index))
	}
}

func FuzzDecodeRequest(f *testing.F) {
	for _, request := range requests() {
		var wire ipc.Message
		EncodeRequest(&request, 6, &wire)
		f.Add(wire.Payload[:wire.PayloadSize])
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 6}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		var decoded Request
		correlation, ok := DecodeRequest(&wire, &decoded)
		if !ok {
			return
		}
		if _, _, attributable := RequestOpcode(&wire); !attributable {
			t.Fatal("decodable request is not attributable")
		}
		var again ipc.Message
		if !EncodeRequest(&decoded, correlation, &again) || again != wire {
			t.Fatalf("%+v does not re-encode", decoded)
		}
	})
}

func FuzzDecodeReply(f *testing.F) {
	for _, reply := range replies() {
		var wire ipc.Message
		EncodeReply(&reply, 6, &wire)
		f.Add(wire.Payload[:wire.PayloadSize])
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: 6}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		opcode := Opcode(0)
		if len(payload) > 0 {
			opcode = Opcode(payload[0])
		}
		var decoded Reply
		if !DecodeReply(&wire, 6, opcode, &decoded) {
			return
		}
		var again ipc.Message
		if !EncodeReply(&decoded, 6, &again) || again != wire {
			t.Fatalf("%+v does not re-encode", decoded)
		}
	})
}

func FuzzDecodeListReply(f *testing.F) {
	records := listRecords()
	seeds := []struct {
		op      Opcode
		records []Record
		seq     uint64
		more    bool
	}{
		{AdminListAccounts, records, 0, true},
		{AdminListAccounts, nil, 3, false},
		{AdminListGroups, []Record{{Kind: RecordGroup, GroupID: 5, GID: 1000, Epoch: testEpoch(), Revision: 9, Name: []byte("wheel")}}, 0, false},
	}
	for _, seed := range seeds {
		var wire ipc.Message
		EncodeListReply(seed.op, ResultOK, seed.seq, seed.more, seed.records, 6, &wire)
		f.Add(wire.Payload[:wire.PayloadSize])
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) == 0 {
			return
		}
		opcode := Opcode(payload[0])
		if !isListOpcode(opcode) {
			return
		}
		for _, base := range []ipc.Message{
			{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: 6},
			{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, Correlation: 0},
		} {
			wire := base
			wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
			result, seq, more, records, ok := DecodeListReply(&wire, 6, opcode)
			if !ok {
				continue
			}
			var again ipc.Message
			if !EncodeListReply(opcode, result, seq, more, records, 6, &again) || again != wire {
				t.Fatalf("list does not re-encode")
			}
			return
		}
	})
}
