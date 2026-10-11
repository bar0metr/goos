package config

const HostnameKey = "system.hostname"

// ValidHostname accepts a single ASCII label, not a DNS name or Unicode text.
func ValidHostname(name []byte) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		if c != '-' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validSetting(key []byte, value Value) bool {
	return !equal(key, []byte(HostnameKey)) || value.Kind == KindText && value.valid() && ValidHostname(value.Bytes())
}

// ServeDurable runs in the service's serialized loop. The persistence callback
// returns OK after acknowledged replacement, PersistFailed before commit, or
// PersistUnknown after an ambiguous replacement. Failure restores RAM/revision;
// unknown retains the tentative candidate for an idempotent forward retry.
// Callers must repeat the same durable mutation to confirm an unknown outcome.
func (store *Store) ServeDurable(scope Scope, rights Rights, request Request, persist func(*Store) Status) Reply {
	reply := Reply{Operation: request.Operation, Status: StatusPermissionDenied}
	if request.Operation != OperationSetDurable {
		reply.Status = StatusInvalidArgument
		return reply
	}
	if rights&DurableWrite == 0 || !scope.exact || !scope.covers([]byte(HostnameKey)) {
		return reply
	}
	if !equal(request.Key, []byte(HostnameKey)) || !scope.covers(request.Key) {
		reply.Status = StatusNotFound
		return reply
	}
	if persist == nil {
		reply.Status = StatusPersistFailed
		return reply
	}
	slot := store.find(request.Key)
	if slot == nil {
		reply.Status = StatusNotFound
		return reply
	}
	previous, revision, slotRevision := slot.value, store.revision, slot.revision
	reply.Status = store.Set(scope, Write, request.Key, request.Value)
	if reply.Status != StatusOK {
		return reply
	}
	switch status := persist(store); status {
	case StatusOK:
	case StatusPersistUnknown:
		reply.Status = StatusPersistUnknown
	default:
		slot.value, store.revision, slot.revision = previous, revision, slotRevision
		reply.Status = StatusPersistFailed
	}
	return reply
}
