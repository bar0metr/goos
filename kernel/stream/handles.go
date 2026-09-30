package stream

import "goos/kernel/handle"

// The functions here are the whole of the kernel's handle accounting for
// stream endpoints (ADR 0089). The invariant they keep is that an end's count
// equals the number of open handles and shared Linux descriptions that name it:
//
//   - StreamCreate installs one handle per end, which Reset counts.
//   - A handle placed into a table by a capability transfer — an IPC
//     attachment or a launch grant — is counted by Placed.
//   - A handle closed by HandleClose, by a move, by a consumed grant, or by the
//     teardown of its process is forgotten by CloseHandle or Drop.
//   - A Linux launch moves its standard-stream references from native handles
//     to shared descriptions. The last descriptor releases each reference.
//
// A move is a Placed for the receiver's copy followed by a CloseHandle of the
// sender's, so it changes nothing net, and the copy is counted first so the end
// cannot close in between.

// Closure describes the end a closed handle named, so the caller can wake the
// peer's waiters and release the slot when the end has closed.
type Closure struct {
	Stream uint64
	Writer bool
	// Closed is set when this was the end's last handle.
	Closed bool
}

// Placed counts a capability just installed in a handle table. Handles that
// are not streams are not counted and always succeed.
func (registry *Registry) Placed(typeID handle.Type, object uint64) bool {
	return typeID != handle.TypeStream || registry.Retain(object)
}

// CloseHandle closes id in table, forgetting its reference first when it names
// a stream endpoint. It reports false, and closes nothing, for a handle that is
// not open or a stream handle whose end has no reference counted.
func (registry *Registry) CloseHandle(table *handle.Table, id handle.ID) (Closure, bool) {
	typeID, cap, found := table.Describe(id)
	if !found {
		return Closure{}, false
	}
	var closure Closure
	if typeID == handle.TypeStream {
		streamID, writer, closed, ok := registry.Drop(cap.Object())
		if !ok {
			return Closure{}, false
		}
		closure = Closure{Stream: streamID, Writer: writer, Closed: closed}
	}
	if !table.Close(id) {
		return Closure{}, false
	}
	return closure, true
}
