package shell

import (
	"goos/internal/logfmt"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

const LogUsage = "log [-f] [-n N] [--level debug|info|warn|error] [--component NAME]"
const LogBatchWork = 8

// LogOptions borrows Component from the invocation's stable argument storage.
// Limit is clamped to ring capacity: requesting more cannot retain more history.
type LogOptions struct {
	Limit     int
	Level     logfmt.Level
	Component []byte
	Follow    bool
}

func ParseLog(invocation Invocation) (LogOptions, bool) {
	if invocation.Count < 1 || invocation.Count > len(invocation.Arguments) {
		return LogOptions{}, false
	}
	options := LogOptions{Limit: kcall.LogReadMax}
	seen := uint8(0)
	for index := 1; index < int(invocation.Count); index++ {
		argument := invocation.Arguments[index]
		if equal(argument, "-f") {
			if seen&1 != 0 {
				return LogOptions{}, false
			}
			seen |= 1
			options.Follow = true
			continue
		}
		flag := uint8(0)
		if equal(argument, "-n") {
			flag = 2
		}
		if equal(argument, "--level") {
			flag = 4
		}
		if equal(argument, "--component") {
			flag = 8
		}
		if flag == 0 || seen&flag != 0 || index+1 >= int(invocation.Count) {
			return LogOptions{}, false
		}
		seen |= flag
		index++
		value := invocation.Arguments[index]
		switch flag {
		case 2:
			limit, valid := logCount(value)
			if !valid {
				return LogOptions{}, false
			}
			options.Limit = limit
		case 4:
			switch {
			case equal(value, "debug"):
				options.Level = logfmt.Debug
			case equal(value, "info"):
				options.Level = logfmt.Info
			case equal(value, "warn"):
				options.Level = logfmt.Warn
			case equal(value, "error"):
				options.Level = logfmt.Error
			default:
				return LogOptions{}, false
			}
		case 8:
			if len(value) == 0 || len(value) > logfmt.MaxComponent {
				return LogOptions{}, false
			}
			for _, character := range value {
				if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.') {
					return LogOptions{}, false
				}
			}
			options.Component = value
		}
	}
	return options, true
}

func logCount(value []byte) (int, bool) {
	if len(value) == 0 {
		return 0, false
	}
	count := uint64(0)
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		digit := uint64(character - '0')
		if count > (^uint64(0)-digit)/10 {
			return 0, false
		}
		count = count*10 + digit
	}
	if count > kcall.LogReadMax {
		return kcall.LogReadMax, true
	}
	return int(count), true
}

func (options LogOptions) Matches(record logfmt.Record) bool {
	if options.Level != 0 && options.Level != record.Level {
		return false
	}
	if len(options.Component) != 0 {
		if len(options.Component) != len(record.Component) {
			return false
		}
		for index := range options.Component {
			if options.Component[index] != record.Component[index] {
				return false
			}
		}
	}
	return true
}

// SelectLog chooses newest matching records from one snapshot, chronological
// when emitted. The caller owns the small index list; record data stays borrowed.
func SelectLog(batch sys.LogBatch, options LogOptions, target *[kcall.LogReadMax]uint8) int {
	if target == nil || options.Limit <= 0 {
		return 0
	}
	count := 0
	for index := batch.Count - 1; index >= 0 && count < options.Limit && count < len(target); index-- {
		record, valid := batch.Record(index)
		if valid && options.Matches(record) {
			target[count] = uint8(index)
			count++
		}
	}
	for left, right := 0, count-1; left < right; left, right = left+1, right-1 {
		target[left], target[right] = target[right], target[left]
	}
	return count
}
