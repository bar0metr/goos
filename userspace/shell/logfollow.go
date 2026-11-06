package shell

import (
	"goos/internal/logfmt"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// LogHooks keeps terminal and native calls outside the bounded follow loop.
// Read with snapshot=true starts at the current Oldest, including across
// sequence wrap. Read returns borrowed records; Write and Gap finish before Read is
// called again. Check drains control/input without blocking. Wait must combine
// the log cursor and terminal cancellation in one blocking operation.
type LogHooks struct {
	Read  func(cursor uint64, snapshot bool) (sys.LogBatch, bool)
	Write func(logfmt.Record) bool
	Gap   func(lost uint64) bool
	Check func() (status uint64, stopped bool)
	Wait  func(cursor uint64) bool
}

// RunLog returns 0 on completion, 1 on read/write/wait failure, or the status
// from Check (130 for Ctrl-C). The fixed snapshot storage lives with Read.
func RunLog(options LogOptions, hooks LogHooks, selected *[kcall.LogReadMax]uint8) uint64 {
	if options.Follow {
		if status, stopped := hooks.Check(); stopped {
			return status
		}
	}
	batch, ok := hooks.Read(0, true)
	if !ok {
		return 1
	}
	var cursor uint64
	if options.Limit > kcall.LogReadMax {
		status, next, stopped := runLogRetained(options, hooks, batch)
		if stopped || !options.Follow {
			return status
		}
		cursor = next
	} else {
		count := SelectLog(batch, options, selected)
		for index := 0; index < count; index++ {
			if options.Follow && index%LogBatchWork == 0 {
				if status, stopped := hooks.Check(); stopped {
					return status
				}
			}
			record, valid := batch.Record(int(selected[index]))
			if !valid || !hooks.Write(record) {
				return 1
			}
		}
		if !options.Follow {
			return 0
		}
		cursor = batch.Next()
	}
	for {
		if status, stopped := hooks.Check(); stopped {
			return status
		}
		batch, ok = hooks.Read(cursor, false)
		if !ok {
			return 1
		}
		if batch.Gap && !hooks.Gap(batch.First-cursor) {
			return 1
		}
		for index := 0; index < batch.Count; index++ {
			if index%LogBatchWork == 0 {
				if status, stopped := hooks.Check(); stopped {
					return status
				}
			}
			record, valid := batch.Record(index)
			if !valid {
				return 1
			}
			if options.Matches(record) && !hooks.Write(record) {
				return 1
			}
		}
		cursor = batch.Next()
		if status, stopped := hooks.Check(); stopped {
			return status
		}
		if !hooks.Wait(cursor) {
			return 1
		}
	}
}

// runLogRetained prints the matching records among the newest
// options.Limit the ring retained when the snapshot was taken, oldest first,
// one batch at a time. A record the ring overwrote while the walk ran is
// reported through Gap. It returns the status, the cursor after the
// snapshot's end, and whether the run must stop with that status.
func runLogRetained(options LogOptions, hooks LogHooks, newest sys.LogBatch) (uint64, uint64, bool) {
	end := newest.Total
	cursor := newest.Oldest
	if retained := end - newest.Oldest; retained > uint64(options.Limit) {
		cursor = end - uint64(options.Limit)
	}
	for cursor != end {
		batch, ok := hooks.Read(cursor, false)
		if !ok {
			return 1, cursor, true
		}
		if batch.Gap && !hooks.Gap(batch.First-cursor) {
			return 1, cursor, true
		}
		if batch.Count == 0 {
			break
		}
		for index := 0; index < batch.Count && batch.First+uint64(index) != end; index++ {
			if options.Follow && index%LogBatchWork == 0 {
				if status, stopped := hooks.Check(); stopped {
					return status, cursor, true
				}
			}
			record, valid := batch.Record(index)
			if !valid {
				return 1, cursor, true
			}
			if options.Matches(record) && !hooks.Write(record) {
				return 1, cursor, true
			}
		}
		next := batch.Next()
		if logfmt.SequenceBefore(end, next) {
			next = end
		}
		cursor = next
	}
	return 0, cursor, false
}
