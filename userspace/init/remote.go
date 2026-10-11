package supervisor

import "goos/userspace/remoteapi"

type RemoteState uint8

const (
	RemoteFree RemoteState = iota
	RemoteHandshake
	RemoteAuthenticated
	RemoteOpening
	RemoteOpen
	RemoteClosing
)

// RemoteRecord belongs to PID 1. A worker cannot select another owner's slot.
type RemoteRecord struct {
	ID            uint64
	Owner         uint64
	Policy        uint64
	State         RemoteState
	Authenticated bool
	Mode          remoteapi.ExecutionMode
	Geometry      remoteapi.Geometry
}

type remoteSlot struct {
	record     RemoteRecord
	generation uint32
	retired    bool
}

// RemoteRegistry bounds handshakes separately from authenticated sessions.
// Closing records retain capacity until PID 1 finishes process and job cleanup.
type RemoteRegistry struct {
	slots [remoteapi.MaxWorkers]remoteSlot
}

func (registry *RemoteRegistry) Admit(owner, policy uint64) (uint64, remoteapi.Result) {
	if owner == 0 || policy == 0 {
		return 0, remoteapi.ResultInvalidArgument
	}
	handshakes := 0
	for index := range registry.slots {
		record := &registry.slots[index].record
		if record.State != RemoteFree && record.Owner == owner {
			return 0, remoteapi.ResultInvalidArgument
		}
		if record.State == RemoteHandshake || record.State == RemoteClosing && !record.Authenticated {
			handshakes++
		}
	}
	if handshakes >= remoteapi.MaxHandshakes {
		return 0, remoteapi.ResultLimit
	}
	for index := range registry.slots {
		slot := &registry.slots[index]
		if slot.record.State != RemoteFree || slot.retired {
			continue
		}
		slot.generation++
		if slot.generation == 0 {
			slot.retired = true
			continue
		}
		id := uint64(slot.generation)<<32 | uint64(index+1)
		slot.record = RemoteRecord{ID: id, Owner: owner, Policy: policy, State: RemoteHandshake}
		return id, remoteapi.ResultOK
	}
	return 0, remoteapi.ResultLimit
}

func (registry *RemoteRegistry) Authenticate(id, owner uint64, limit int) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	if record.State != RemoteHandshake || limit < 1 || limit > remoteapi.MaxSessions {
		return remoteapi.ResultInvalidArgument
	}
	if registry.Authenticated() >= limit {
		return remoteapi.ResultLimit
	}
	record.State = RemoteAuthenticated
	record.Authenticated = true
	return remoteapi.ResultOK
}

// BeginOpen reserves the slot before PID 1 allocates shell resources.
func (registry *RemoteRegistry) BeginOpen(id, owner uint64, geometry remoteapi.Geometry) remoteapi.Result {
	return registry.BeginOpenMode(id, owner, remoteapi.ModeTerminal, geometry)
}

// CheckOpen validates ownership and session state before the caller stores a
// command snapshot. A rejected duplicate must not alter a pending launch.
func (registry *RemoteRegistry) CheckOpen(id, owner uint64, mode remoteapi.ExecutionMode, geometry remoteapi.Geometry) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	if record.State != RemoteAuthenticated || mode > remoteapi.ModeExec || mode == remoteapi.ModeTerminal && !geometry.Valid() || mode == remoteapi.ModeExec && geometry != (remoteapi.Geometry{}) {
		return remoteapi.ResultInvalidArgument
	}
	return remoteapi.ResultOK
}

func (registry *RemoteRegistry) BeginOpenMode(id, owner uint64, mode remoteapi.ExecutionMode, geometry remoteapi.Geometry) remoteapi.Result {
	if result := registry.CheckOpen(id, owner, mode, geometry); result != remoteapi.ResultOK {
		return result
	}
	record, _ := registry.owned(id, owner)
	record.Geometry = geometry
	record.Mode = mode
	record.State = RemoteOpening
	return remoteapi.ResultOK
}

func (registry *RemoteRegistry) CommitOpen(id, owner uint64) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	if record.State != RemoteOpening {
		return remoteapi.ResultInvalidArgument
	}
	record.State = RemoteOpen
	return remoteapi.ResultOK
}

func (registry *RemoteRegistry) Resize(id, owner uint64, geometry remoteapi.Geometry) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	if record.State != RemoteOpen || record.Mode != remoteapi.ModeTerminal || !geometry.Valid() {
		return remoteapi.ResultInvalidArgument
	}
	if geometry.Sequence <= record.Geometry.Sequence {
		return remoteapi.ResultOK
	}
	record.Geometry = geometry
	return remoteapi.ResultOK
}

// BeginClose is idempotent. It does not release the slot's capacity.
func (registry *RemoteRegistry) BeginClose(id, owner uint64) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	record.State = RemoteClosing
	return remoteapi.ResultOK
}

// FinishClose runs after PID 1 has reaped jobs and released session resources.
func (registry *RemoteRegistry) FinishClose(id, owner uint64) remoteapi.Result {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return result
	}
	if record.State != RemoteClosing {
		return remoteapi.ResultInvalidArgument
	}
	*record = RemoteRecord{}
	return remoteapi.ResultOK
}

func (registry *RemoteRegistry) Snapshot(id, owner uint64) (RemoteRecord, remoteapi.Result) {
	record, result := registry.owned(id, owner)
	if result != remoteapi.ResultOK {
		return RemoteRecord{}, result
	}
	return *record, remoteapi.ResultOK
}

func (registry *RemoteRegistry) Authenticated() int {
	count := 0
	for index := range registry.slots {
		record := &registry.slots[index].record
		if record.Authenticated {
			count++
		}
	}
	return count
}

func (registry *RemoteRegistry) owned(id, owner uint64) (*RemoteRecord, remoteapi.Result) {
	index := uint32(id)
	if id>>32 == 0 || index == 0 || index > uint32(len(registry.slots)) {
		return nil, remoteapi.ResultNotFound
	}
	record := &registry.slots[index-1].record
	if record.State == RemoteFree || record.ID != id {
		return nil, remoteapi.ResultNotFound
	}
	if owner == 0 || record.Owner != owner {
		return nil, remoteapi.ResultPermissionDenied
	}
	return record, remoteapi.ResultOK
}
