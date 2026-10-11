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
	cursor := batch.Total
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
		cursor = batch.Total
		if status, stopped := hooks.Check(); stopped {
			return status
		}
		if !hooks.Wait(cursor) {
			return 1
		}
	}
}
