package network

// tcpTimers is a binary min-heap of deadlines with at most one per ref. A ref
// is an endpoint slot or a tombstone index. where records each ref's heap
// position, so moving or dropping a deadline costs a sift, not a search, and
// the earliest deadline is the root.
type tcpTimers struct {
	nodes []tcpTimer
	// where[ref] is the ref's heap position plus one, zero when it has no
	// deadline.
	where []int32
}

type tcpTimer struct {
	at  uint64
	ref int32
}

func (timers *tcpTimers) reset() {
	timers.nodes = timers.nodes[:0]
	clear(timers.where)
}

// set gives ref the deadline at, or none when at is zero.
func (timers *tcpTimers) set(ref int32, at uint64) {
	for int(ref) >= len(timers.where) {
		timers.where = append(timers.where, 0)
	}
	position := int(timers.where[ref]) - 1
	switch {
	case at == 0:
		if position >= 0 {
			timers.removeAt(position)
		}
	case position < 0:
		timers.nodes = append(timers.nodes, tcpTimer{at: at, ref: ref})
		position = len(timers.nodes) - 1
		timers.where[ref] = int32(position + 1)
		timers.up(position)
	default:
		old := timers.nodes[position].at
		timers.nodes[position].at = at
		if at < old {
			timers.up(position)
		} else if at > old {
			timers.down(position)
		}
	}
}

// at is ref's deadline, zero when it has none.
func (timers *tcpTimers) at(ref int32) uint64 {
	if int(ref) >= len(timers.where) || timers.where[ref] == 0 {
		return 0
	}
	return timers.nodes[timers.where[ref]-1].at
}

// earliest is the ref with the first deadline, and the deadline.
func (timers *tcpTimers) earliest() (int32, uint64, bool) {
	if len(timers.nodes) == 0 {
		return 0, 0, false
	}
	return timers.nodes[0].ref, timers.nodes[0].at, true
}

func (timers *tcpTimers) removeAt(position int) {
	last := len(timers.nodes) - 1
	timers.where[timers.nodes[position].ref] = 0
	if position != last {
		timers.nodes[position] = timers.nodes[last]
		timers.where[timers.nodes[position].ref] = int32(position + 1)
	}
	timers.nodes = timers.nodes[:last]
	if position != last {
		timers.down(position)
		timers.up(position)
	}
}

func (timers *tcpTimers) swap(a, b int) {
	timers.nodes[a], timers.nodes[b] = timers.nodes[b], timers.nodes[a]
	timers.where[timers.nodes[a].ref] = int32(a + 1)
	timers.where[timers.nodes[b].ref] = int32(b + 1)
}

func (timers *tcpTimers) up(position int) {
	for position > 0 {
		parent := (position - 1) / 2
		if timers.nodes[parent].at <= timers.nodes[position].at {
			return
		}
		timers.swap(parent, position)
		position = parent
	}
}

func (timers *tcpTimers) down(position int) {
	for {
		child := 2*position + 1
		if child >= len(timers.nodes) {
			return
		}
		if right := child + 1; right < len(timers.nodes) && timers.nodes[right].at < timers.nodes[child].at {
			child = right
		}
		if timers.nodes[position].at <= timers.nodes[child].at {
			return
		}
		timers.swap(position, child)
		position = child
	}
}

// valid reports whether the heap order and the position records agree.
func (timers *tcpTimers) valid() bool {
	for position := range timers.nodes {
		if parent := (position - 1) / 2; position > 0 && timers.nodes[parent].at > timers.nodes[position].at {
			return false
		}
		ref := timers.nodes[position].ref
		if int(ref) >= len(timers.where) || timers.where[ref] != int32(position+1) {
			return false
		}
	}
	return true
}
