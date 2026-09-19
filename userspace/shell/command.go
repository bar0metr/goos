// Package shell implements allocation-free parsing for the native command loop.
package shell

import execproto "goos/userspace/exec"

const (
	MaxArguments    = 8
	MaxArgumentSize = 63
	MaxCommands     = 8
)

// ParseSequence validates the whole line before exposing any command for
// execution. Returned slices borrow line; scratch is reused for validation.
// A trailing separator is allowed, but empty commands between separators are not.
func ParseSequence(line []byte, scratch *[MaxArguments][MaxArgumentSize]byte) ([MaxCommands][]byte, int, bool) {
	var commands [MaxCommands][]byte
	count, start := 0, 0
	quote, escaped := byte(0), false
	for index := 0; index <= len(line); index++ {
		atEnd := index == len(line)
		if !atEnd {
			value := line[index]
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
				continue
			}
			if value == '\'' || value == '"' {
				if quote == 0 {
					quote = value
				} else if quote == value {
					quote = 0
				}
				continue
			}
			if value != ';' || quote != 0 {
				continue
			}
		}
		if quote != 0 || escaped {
			return commands, 0, false
		}
		command := trimSpaces(line[start:index])
		if len(command) == 0 {
			return commands, count, atEnd
		}
		if count == MaxCommands {
			return commands, 0, false
		}
		pipeline, _, redirected, _, ok := SplitOutputRedirect(command)
		if !ok {
			return commands, 0, false
		}
		stages, stageCount, ok := SplitPipeline(pipeline)
		if !ok {
			return commands, 0, false
		}
		if redirected && stageCount == execproto.MaxStages {
			return commands, 0, false
		}
		for _, stage := range stages[:stageCount] {
			if len(stage) == 0 {
				continue
			}
			invocation, valid := ParseInvocation(stage, scratch)
			if !valid {
				return commands, 0, false
			}
			name := invocation.Arguments[0]
			if (stageCount > 1 || redirected) && (equal(name, "cd") || equal(name, "help") || equal(name, "clear")) {
				return commands, 0, false
			}
		}
		commands[count] = command
		count++
		start = index + 1
	}
	return commands, count, true
}

// SplitOutputRedirect separates one unquoted output redirection. The target is
// left encoded so ParseInvocation can apply the normal quoting and escaping
// rules. Multiple targets are rejected.
func SplitOutputRedirect(line []byte) (command, target []byte, redirected, appendMode, ok bool) {
	quote, escaped, separator, operatorSize := byte(0), false, -1, 1
	for index, value := range line {
		if escaped {
			escaped = false
			continue
		}
		if value == '\\' {
			escaped = true
			continue
		}
		if value == '\'' || value == '"' {
			if quote == 0 {
				quote = value
			} else if quote == value {
				quote = 0
			}
			continue
		}
		if value == '<' && quote == 0 {
			return nil, nil, false, false, false
		}
		if value == '>' && quote == 0 {
			if separator >= 0 && operatorSize == 2 && index == separator+1 {
				continue
			}
			if separator >= 0 {
				return nil, nil, false, false, false
			}
			if index > 0 && line[index-1] == '2' && (index == 1 || line[index-2] == ' ') {
				return nil, nil, false, false, false
			}
			separator = index
			if index+1 < len(line) && line[index+1] == '>' {
				operatorSize = 2
				appendMode = true
			}
		}
	}
	if quote != 0 || escaped {
		return nil, nil, false, false, false
	}
	if separator < 0 {
		line = trimSpaces(line)
		return line, nil, false, false, len(line) != 0
	}
	command, target = trimSpaces(line[:separator]), trimSpaces(line[separator+operatorSize:])
	if len(command) == 0 || len(target) == 0 || target[0] == '>' {
		return nil, nil, false, false, false
	}
	var storage [MaxArguments][MaxArgumentSize]byte
	invocation, valid := ParseInvocation(target, &storage)
	if !valid || invocation.Count != 1 {
		return nil, nil, false, false, false
	}
	return command, target, true, appendMode, true
}

type Invocation struct {
	Arguments [MaxArguments][]byte
	Count     uint8
}

// SplitPipeline returns up to MaxStages invocations separated by unquoted
// pipes. Empty stages, unmatched quotes, and trailing escapes are rejected.
func SplitPipeline(line []byte) ([execproto.MaxStages][]byte, int, bool) {
	var stages [execproto.MaxStages][]byte
	quote, escaped := byte(0), false
	start, count := 0, 0
	for index := 0; index <= len(line); index++ {
		if index < len(line) {
			value := line[index]
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
				continue
			}
			if value == '\'' || value == '"' {
				if quote == 0 {
					quote = value
				} else if quote == value {
					quote = 0
				}
				continue
			}
			if value != '|' || quote != 0 {
				continue
			}
		}
		if quote != 0 || escaped || count == len(stages) {
			return stages, 0, false
		}
		stage := trimSpaces(line[start:index])
		if len(stage) == 0 {
			return stages, 0, false
		}
		stages[count] = stage
		count++
		start = index + 1
	}
	return stages, count, true
}

func trimSpaces(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && value[start] == ' ' {
		start++
	}
	for end > start && value[end-1] == ' ' {
		end--
	}
	return value[start:end]
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
