package config

func (store *Store) advanceRevision(slot *entry) Status {
	if store.revision == ^uint64(0) {
		return StatusResourceExhausted
	}
	store.revision++
	slot.revision = store.revision
	return StatusOK
}

// Revision lets a reader bracket a candidate's Get requests. An unchanged
// scoped revision proves that no covered value changed between the reads.
func (store *Store) Revision(scope Scope, rights Rights) (uint64, Status) {
	if rights&Read == 0 {
		return 0, StatusPermissionDenied
	}
	var revision uint64
	for index := range store.entries {
		slot := &store.entries[index]
		if slot.used && scope.covers(slot.key[:slot.size]) && slot.revision > revision {
			revision = slot.revision
		}
	}
	return revision, StatusOK
}

func validRevisionRequest(request *Request) bool {
	return len(request.Key) == 0 && request.Value == (Value{}) && request.Cursor == 0
}

func validRevisionReply(reply *Reply) bool {
	if len(reply.Key) != 0 || reply.ReadOnly || reply.Cursor != 0 {
		return false
	}
	if reply.Status == StatusOK {
		return reply.Value.Kind == KindUint && reply.Value.Size == 0
	}
	return reply.Value == (Value{})
}
