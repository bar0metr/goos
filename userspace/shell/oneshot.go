package shell

import "goos/userspace/startup"

const (
	StatusFailure       = uint64(1)
	StatusUsage         = uint64(2)
	StatusCannotExecute = uint64(126)
	StatusNotFound      = uint64(127)
	StatusInterrupted   = uint64(130)
)

// OneShotCommand recognizes only shell -c COMMAND. The command borrows startup.
// Interactive startup has exactly argv[0] and still requires a terminal.
func OneShotCommand(data startup.Data) (command []byte, oneShot, valid bool) {
	if data.Args() == 1 {
		return nil, false, true
	}
	if data.Args() != 3 {
		return nil, false, false
	}
	flag, _ := data.Arg(1)
	command, _ = data.Arg(2)
	if len(flag) != 2 || flag[0] != '-' || flag[1] != 'c' || len(command) == 0 || len(command) > MaxLine {
		return nil, false, false
	}
	return command, true, true
}

// ParseOneShot validates every command before execution. Background operators
// are forbidden anywhere outside quotes or escapes, not just at the line end.
func ParseOneShot(line []byte, scratch *Storage) ([MaxCommands][]byte, int, bool) {
	var empty [MaxCommands][]byte
	if len(line) == 0 || len(line) > MaxLine {
		return empty, 0, false
	}
	quote, escaped := byte(0), false
	for _, value := range line {
		if value == 0 {
			return empty, 0, false
		}
		switch {
		case escaped:
			escaped = false
		case value == '\\':
			escaped = true
		case value == '\'' || value == '"':
			if quote == 0 {
				quote = value
			} else if quote == value {
				quote = 0
			}
		case value == '&' && quote == 0:
			return empty, 0, false
		}
	}
	// Execution removes leading time wrappers before parsing the sequence.
	for {
		command, timed := SplitTime(line)
		if !timed {
			break
		}
		line = command
	}
	commands, count, valid := ParseSequence(line, scratch)
	if !valid || count == 0 {
		return empty, 0, false
	}
	return commands, count, true
}
