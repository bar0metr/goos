// Package shell implements allocation-free parsing for the native command loop.
package shell

const (
	MaxArguments    = 8
	MaxArgumentSize = 63
)

type Invocation struct {
	Arguments [MaxArguments][]byte
	Count     uint8
}

// ParseInvocation tokenizes a bounded command line with single quotes, double
// quotes, and backslash escaping. The returned slices refer to storage.
func ParseInvocation(line []byte, storage *[MaxArguments][MaxArgumentSize]byte) (Invocation, bool) {
	var result Invocation
	if storage == nil {
		return result, false
	}
	position := 0
	for position < len(line) {
		for position < len(line) && line[position] == ' ' {
			position++
		}
		if position == len(line) {
			break
		}
		if result.Count == MaxArguments {
			return Invocation{}, false
		}
		index, quote := int(result.Count), byte(0)
		size := 0
		for position < len(line) {
			value := line[position]
			if quote == 0 && value == ' ' {
				break
			}
			if value == '\\' {
				position++
				if position == len(line) {
					return Invocation{}, false
				}
				value = line[position]
			} else if value == '\'' || value == '"' {
				if quote == 0 {
					quote = value
					position++
					continue
				}
				if quote == value {
					quote = 0
					position++
					continue
				}
			}
			if size == MaxArgumentSize {
				return Invocation{}, false
			}
			storage[index][size] = value
			size++
			position++
		}
		if quote != 0 || size == 0 {
			return Invocation{}, false
		}
		result.Arguments[index] = storage[index][:size]
		result.Count++
	}
	return result, result.Count != 0
}

type Command uint8

const (
	CommandEmpty Command = iota
	CommandHelp
	CommandEcho
	CommandClear
	CommandUptime
	CommandPwd
	CommandLs
	CommandCd
	CommandCat
	CommandUnknown
)

func Parse(line []byte) (Command, []byte) {
	switch {
	case len(line) == 0:
		return CommandEmpty, nil
	case equal(line, "help"):
		return CommandHelp, nil
	case equal(line, "echo"):
		return CommandEcho, nil
	case hasPrefix(line, "echo "):
		return CommandEcho, line[5:]
	case equal(line, "clear"):
		return CommandClear, nil
	case equal(line, "uptime"):
		return CommandUptime, nil
	case equal(line, "pwd"):
		return CommandPwd, nil
	case equal(line, "ls"):
		return CommandLs, nil
	case hasPrefix(line, "ls ") && len(line) > 3:
		return CommandLs, line[3:]
	case equal(line, "cd"):
		return CommandCd, nil
	case hasPrefix(line, "cd ") && len(line) > 3:
		return CommandCd, line[3:]
	case hasPrefix(line, "cat ") && len(line) > 4:
		return CommandCat, line[4:]
	default:
		return CommandUnknown, nil
	}
}

// SplitInvocation returns one bounded command token and its unparsed argument
// tail. Quoting and pipelines belong to later shell grammar iterations.
func SplitInvocation(line []byte) ([]byte, []byte) {
	start := 0
	for start < len(line) && line[start] == ' ' {
		start++
	}
	end := start
	for end < len(line) && line[end] != ' ' {
		end++
	}
	arguments := end
	for arguments < len(line) && line[arguments] == ' ' {
		arguments++
	}
	return line[start:end], line[arguments:]
}

func equal(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}

func hasPrefix(value []byte, prefix string) bool {
	return len(value) >= len(prefix) && equal(value[:len(prefix)], prefix)
}
