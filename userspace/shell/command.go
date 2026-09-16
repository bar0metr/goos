// Package shell implements allocation-free parsing for the native command loop.
package shell

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
