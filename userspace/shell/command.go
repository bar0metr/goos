// Package shell implements allocation-free parsing for the native command loop.
package shell

type Command uint8

const (
	CommandEmpty Command = iota
	CommandHelp
	CommandEcho
	CommandClear
	CommandUptime
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
	default:
		return CommandUnknown, nil
	}
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
