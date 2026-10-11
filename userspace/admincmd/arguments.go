package admincmd

type Arguments interface {
	ArgCount() int
	Arg(int) ([]byte, bool)
}

func LinesArguments(arguments Arguments) (uint64, int, bool) {
	first, _ := arguments.Arg(1)
	if same(first, []byte("-n")) {
		if arguments.ArgCount() != 3 {
			return 0, 0, false
		}
		text, _ := arguments.Arg(2)
		count, ok := Count(text)
		return count, 3, ok
	}
	return 10, 1, arguments.ArgCount() == 1
}

func GrepArguments(arguments Arguments) ([]byte, bool, int, bool) {
	index := 1
	pattern, _ := arguments.Arg(index)
	insensitive := same(pattern, []byte("-i"))
	if insensitive {
		index++
		pattern, _ = arguments.Arg(index)
	}
	remaining := arguments.ArgCount() - index
	file := 0
	if remaining == 1 {
		file = index + 1
	}
	return pattern, insensitive, file, remaining >= 0 && remaining <= 1
}

func WCArguments(arguments Arguments) (byte, int, bool) {
	mode, index := byte(0), 1
	first, _ := arguments.Arg(1)
	if len(first) > 0 && first[0] == '-' {
		if len(first) != 2 || first[1] != 'l' && first[1] != 'w' && first[1] != 'c' {
			return 0, 0, false
		}
		mode, index = first[1], 2
	}
	remaining := arguments.ArgCount() - index + 1
	file := 0
	if remaining == 1 {
		file = index
	}
	return mode, file, remaining >= 0 && remaining <= 1
}

func FindArguments(arguments Arguments) ([]byte, bool) {
	if arguments.ArgCount() == 1 {
		return nil, true
	}
	option, _ := arguments.Arg(2)
	name, _ := arguments.Arg(3)
	if arguments.ArgCount() != 3 || !same(option, []byte("-name")) || len(name) == 0 {
		return nil, false
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			return nil, false
		}
	}
	return name, true
}

func DateArguments(arguments Arguments) bool {
	if arguments.ArgCount() == 0 {
		return true
	}
	argument, _ := arguments.Arg(1)
	return arguments.ArgCount() == 1 && same(argument, []byte("-u"))
}
