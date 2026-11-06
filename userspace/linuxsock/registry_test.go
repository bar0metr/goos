package linuxsock

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

func (h *harness) closeSocket(family int, id uint64) {
	h.t.Helper()
	if reply := h.call(family, linuxsockapi.Request{Opcode: opClose, Socket: id}); reply.Result != linuxsockapi.ResultOK {
		h.t.Fatalf("close %x: %d", id, reply.Result)
	}
}

func (h *harness) openResult(family int, kind uint32) linuxsockapi.Result {
	h.t.Helper()
	reply := h.call(family, linuxsockapi.Request{Opcode: opOpen, Length: kind})
	if reply.Result == linuxsockapi.ResultOK {
		h.t.Fatal("open succeeded")
	}
	if reply.Socket != 0 {
		h.t.Fatalf("refused open named socket %x", reply.Socket)
	}
	return reply.Result
}

// A family fills its quota exactly; the next socket answers FamilyLimit,
// which the kernel reports as EMFILE, without asking netd. Another family
// still allocates, and a released index is the next one taken.
func TestFamilyQuotaIsExact(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	limit := h.svc.Limits().Family
	if limit != DefaultFamilySockets || limit < 512 || h.svc.Limits().System < 1024 {
		t.Fatalf("default limits %+v", h.svc.Limits())
	}
	ids := make([]uint64, limit)
	for index := range ids {
		kind := uint32(linuxsockapi.KindStream)
		if index%2 == 1 {
			kind = linuxsockapi.KindDatagram
		}
		ids[index] = h.open(a, kind)
	}
	if result := h.openResult(a, linuxsockapi.KindDatagram); result != linuxsockapi.ResultFamilyLimit {
		t.Fatalf("past the family quota: %d", result)
	}
	if h.has(NativeOpen) {
		t.Fatal("asked netd for an endpoint past the quota")
	}
	other := h.open(b, linuxsockapi.KindStream)
	if index, _ := linuxsockapi.SocketIndex(other); index != 0 {
		t.Fatalf("other family index %d", index)
	}
	victim := ids[limit/2]
	victimIndex, _ := linuxsockapi.SocketIndex(victim)
	h.closeSocket(a, victim)
	reused := h.open(a, linuxsockapi.KindDatagram)
	if index, _ := linuxsockapi.SocketIndex(reused); index != victimIndex || reused == victim {
		t.Fatalf("reuse %x after %x", reused, victim)
	}
	if result := h.openResult(a, linuxsockapi.KindStream); result != linuxsockapi.ResultFamilyLimit {
		t.Fatalf("full again: %d", result)
	}
	var status linuxsockapi.ControlReply
	h.svc.Status(&status)
	if status.Sockets != uint16(limit+1) || status.FamilySockets[a] != uint16(limit) || status.FamilySockets[b] != 1 ||
		status.SocketLimit != DefaultSystemSockets || status.FamilyLimit != uint16(limit) || status.FamilyReserve != DefaultFamilyReserve {
		t.Fatalf("status %+v", status)
	}
	snap := linuxsockapi.LoadFamilySnapshot(h.rings(a))
	if snap.Sockets != uint16(limit) || snap.FamilyLimit != uint16(limit) || snap.SystemLimit != DefaultSystemSockets ||
		snap.Reserve != DefaultFamilyReserve {
		t.Fatalf("snapshot %+v", snap)
	}
}

// Two families at their quota hold the whole service, every socket's
// readiness entry and snapshot reads back, and a third family finds the
// service full: ENFILE rather than EMFILE.
func TestHundredsOfSocketsAcrossFamilies(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	ids := map[int][]uint64{}
	for _, f := range []int{a, b} {
		for range DefaultFamilySockets {
			ids[f] = append(ids[f], h.open(f, linuxsockapi.KindDatagram))
		}
	}
	if h.svc.sockets.live != DefaultSystemSockets || h.svc.socketCap() != DefaultSystemSockets {
		t.Fatalf("live %d allocated %d", h.svc.sockets.live, h.svc.socketCap())
	}
	for _, f := range []int{a, b} {
		for _, id := range ids[f] {
			if state := h.readiness(f, id); state.Socket != id {
				t.Fatalf("entry of %x reads %x", id, state.Socket)
			}
			index, _ := linuxsockapi.SocketIndex(id)
			page := int(linuxsockapi.SocketSnapshotPage(index))
			if snap := linuxsockapi.LoadSocketSnapshot(h.pools[f][page*4096:(page+1)*4096], index); snap.Socket != id {
				t.Fatalf("snapshot of %x reads %x", id, snap.Socket)
			}
		}
	}
	c := h.family(3, 9)
	if result := h.openResult(c, linuxsockapi.KindStream); result != linuxsockapi.ResultServiceLimit {
		t.Fatalf("full service: %d", result)
	}
	h.closeSocket(a, ids[a][0])
	h.open(c, linuxsockapi.KindStream)
	h.svc.RemoveFamily(b)
	// a holds 511 sockets and c one, 31 short of its reserve.
	if h.svc.sockets.live != DefaultFamilySockets || h.svc.sockets.reserved != DefaultFamilyReserve-1 {
		t.Fatalf("after removal live %d reserved %d", h.svc.sockets.live, h.svc.sockets.reserved)
	}
}

// One family cannot take the room the service keeps back for another.
// With 600 sockets and reserves of 32 for three families, the first family
// stops at its own quota, the second at 600 less the first's 512 and the
// third's reserve, and the third still gets its reserve.
func TestReservesHoldAgainstOtherFamilies(t *testing.T) {
	h := newHarness(t)
	if !h.svc.SetLimits(Limits{System: 600, Family: 512, Reserve: 32}) {
		t.Fatal("limits")
	}
	a := h.family(1, 7)
	b := h.family(2, 8)
	c := h.family(3, 9)
	// fill opens sockets until the service refuses one.
	fill := func(f int) (int, linuxsockapi.Result) {
		for count := 0; ; count++ {
			correlation := h.send(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindDatagram})
			if !h.has(NativeOpen) {
				return count, h.reply(f, correlation).Result
			}
			nextEndpoint++
			h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: nextEndpoint})
			if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
				t.Fatalf("open %d", reply.Result)
			}
		}
	}
	_, result := fill(a)
	if result != linuxsockapi.ResultFamilyLimit || h.svc.families[a].count != 512 {
		t.Fatalf("first family %d %d", h.svc.families[a].count, result)
	}
	_, result = fill(b)
	if result != linuxsockapi.ResultServiceLimit || h.svc.families[b].count != 600-512-32 {
		t.Fatalf("second family %d %d", h.svc.families[b].count, result)
	}
	_, result = fill(c)
	if result != linuxsockapi.ResultServiceLimit || h.svc.families[c].count != 32 {
		t.Fatalf("third family %d %d", h.svc.families[c].count, result)
	}
	if h.svc.sockets.live != 600 {
		t.Fatalf("live %d", h.svc.sockets.live)
	}
}

// A stale identity never reaches the socket that took its index: an older
// generation, a newer one, and another session's all answer Stale. The
// protocol refuses an index past MaxSockets before the service sees it.
func TestStaleIdentitiesStayStale(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	old := h.open(f, linuxsockapi.KindStream)
	h.closeSocket(f, old)
	fresh := h.open(f, linuxsockapi.KindStream)
	oldIndex, _ := linuxsockapi.SocketIndex(old)
	freshIndex, _ := linuxsockapi.SocketIndex(fresh)
	if oldIndex != freshIndex || old == fresh {
		t.Fatalf("index %d %d", oldIndex, freshIndex)
	}
	for _, id := range []uint64{
		old,
		linuxsockapi.MakeSocket(8, uint32(fresh>>linuxsockapi.IndexBits), freshIndex),
		fresh + 1<<linuxsockapi.IndexBits,
	} {
		if reply := h.call(f, linuxsockapi.Request{Opcode: opGetName, Socket: id}); reply.Result != linuxsockapi.ResultStale {
			t.Fatalf("identity %x answered %d", id, reply.Result)
		}
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opGetName, Socket: fresh}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("fresh identity %d", reply.Result)
	}
	// The generation of an index wraps without reaching zero.
	h.svc.families[f].generations[freshIndex] = linuxsockapi.GenerationMask
	h.closeSocket(f, fresh)
	wrapped := h.open(f, linuxsockapi.KindStream)
	if generation := uint32(wrapped>>linuxsockapi.IndexBits) & linuxsockapi.GenerationMask; generation != 1 {
		t.Fatalf("wrapped generation %d", generation)
	}
}

func TestSetLimitsRefusesBadQuotas(t *testing.T) {
	h := newHarness(t)
	for _, l := range []Limits{
		{System: 1024, Family: linuxsockapi.MaxSockets + 1},
		{System: 100, Family: 200},
		{System: MaxSystemSockets + 1, Family: 512},
		{System: 1024, Family: 512, Reserve: 65},
		{System: 1024, Family: 0},
		{System: 1024, Family: 16, Reserve: 17},
	} {
		if h.svc.SetLimits(l) {
			t.Fatalf("accepted %+v", l)
		}
	}
	f := h.family(1, 7)
	h.open(f, linuxsockapi.KindStream)
	if h.svc.SetLimits(Limits{System: 2048, Family: 512, Reserve: 32}) {
		t.Fatal("changed the quotas under an open socket")
	}
	if h.svc.Limits() != DefaultLimits() {
		t.Fatalf("limits %+v", h.svc.Limits())
	}
}

// The tables grow with use, not with the quota.
func TestTablesGrowWithUse(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	if h.svc.socketCap() != 0 {
		t.Fatalf("allocated %d before use", h.svc.socketCap())
	}
	for range socketChunk + 1 {
		h.open(f, linuxsockapi.KindDatagram)
	}
	if h.svc.socketCap() != 2*socketChunk {
		t.Fatalf("allocated %d", h.svc.socketCap())
	}
	if h.svc.opCap() > 2*opChunk {
		t.Fatalf("operations allocated %d", h.svc.opCap())
	}
}
