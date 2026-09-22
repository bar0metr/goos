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
		redirects, ok := SplitRedirections(command)
		if !ok {
			return commands, 0, false
		}
		stages, stageCount, ok := SplitPipeline(redirects.Command)
		if !ok {
			return commands, 0, false
		}
		redirectCount := redirects.Stages()
		if stageCount+redirectCount > execproto.MaxStages {
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
			if (stageCount > 1 || redirectCount != 0) && (equal(name, "cd") || equal(name, "help") || equal(name, "clear") || equal(name, "diag") || equal(name, "time")) {
				return commands, 0, false
			}
		}
		commands[count] = command
		count++
		start = index + 1
	}
	return commands, count, true
}

// SplitTime reports whether line starts with the time builtin and returns the
// command line it measures, which is empty when none was given.
func SplitTime(line []byte) ([]byte, bool) {
	line = trimSpaces(line)
	if !hasPrefix(line, "time") || len(line) > 4 && line[4] != ' ' {
		return nil, false
	}
	return trimSpaces(line[4:]), true
}

type Redirections struct {
	Command     []byte
	Input       []byte
	Output      []byte
	Error       []byte
	Append      bool
	ErrorAppend bool
}

// Stages returns how many pipeline slots the redirections consume: a file
// producer for input plus one write sink per redirected output stream.
func (r Redirections) Stages() int {
	count := 0
	for _, target := range [][]byte{r.Input, r.Output, r.Error} {
		if len(target) != 0 {
			count++
		}
	}
	return count
}

type redirectKind uint8

const (
	redirectInput redirectKind = iota
	redirectOutput
	redirectError
)

type redirectOperator struct {
	offset int
	size   int
	kind   redirectKind
}

// SplitRedirections separates at most one input, one stdout, and one stderr
// redirection. `2>` and `2>>` are recognised only when the digit starts a word.
// Targets remain encoded for ParseInvocation to apply normal argument rules.
func SplitRedirections(line []byte) (Redirections, bool) {
	var operators [3]redirectOperator
	count := 0
	quote, escaped := byte(0), false
	var seen [3]bool
	for index := 0; index < len(line); index++ {
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
		if quote != 0 || value != '<' && value != '>' {
			continue
		}
		operator := redirectOperator{offset: index, size: 1, kind: redirectInput}
		if value == '>' {
			operator.kind = redirectOutput
			if index > 0 && line[index-1] == '2' && (index == 1 || line[index-2] == ' ') {
				operator.offset, operator.size, operator.kind = index-1, 2, redirectError
			}
			if index+1 < len(line) && line[index+1] == '>' {
				operator.size++
				index++
			}
		}
		if seen[operator.kind] || count == len(operators) {
			return Redirections{}, false
		}
		seen[operator.kind] = true
		operators[count] = operator
		count++
	}
	if quote != 0 || escaped {
		return Redirections{}, false
	}
	if count == 0 {
		command := trimSpaces(line)
		return Redirections{Command: command}, len(command) != 0
	}
	result := Redirections{Command: trimSpaces(line[:operators[0].offset])}
	if len(result.Command) == 0 {
		return Redirections{}, false
	}
	var storage [MaxArguments][MaxArgumentSize]byte
	for index := 0; index < count; index++ {
		operator := operators[index]
		start := operator.offset + operator.size
		end := len(line)
		if index+1 < count {
			end = operators[index+1].offset
		}
		target := trimSpaces(line[start:end])
		invocation, valid := ParseInvocation(target, &storage)
		if !valid || invocation.Count != 1 {
			return Redirections{}, false
		}
		switch operator.kind {
		case redirectInput:
			result.Input = target
		case redirectOutput:
			result.Output, result.Append = target, operator.size == 2
		default:
			result.Error, result.ErrorAppend = target, operator.size == 3
		}
	}
	return result, true
}

// SplitOutputRedirect preserves the output-only parser interface.
func SplitOutputRedirect(line []byte) (command, target []byte, redirected, appendMode, ok bool) {
	redirects, ok := SplitRedirections(line)
	if !ok || len(redirects.Input) != 0 || len(redirects.Error) != 0 {
		return nil, nil, false, false, false
	}
	return redirects.Command, redirects.Output, len(redirects.Output) != 0, redirects.Append, true
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
