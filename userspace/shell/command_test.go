package shell

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		line    string
		command Command
		args    string
	}{
		{"", CommandEmpty, ""},
		{"help", CommandHelp, ""},
		{"echo", CommandEcho, ""},
		{"echo hello world", CommandEcho, "hello world"},
		{"clear", CommandClear, ""},
		{"uptime", CommandUptime, ""},
		{"pwd", CommandPwd, ""},
		{"ls", CommandLs, ""},
		{"ls /system", CommandLs, "/system"},
		{"cd ..", CommandCd, ".."},
		{"cat motd", CommandCat, "motd"},
		{"cat", CommandUnknown, ""},
		{"unknown", CommandUnknown, ""},
		{"help now", CommandUnknown, ""},
	}
	for _, test := range tests {
		command, args := Parse([]byte(test.line))
		if command != test.command || string(args) != test.args {
			t.Errorf("Parse(%q) = %d, %q", test.line, command, args)
		}
	}
}

func TestSplitInvocation(t *testing.T) {
	command, arguments := SplitInvocation([]byte("  args   alpha beta"))
	if string(command) != "args" || string(arguments) != "alpha beta" {
		t.Fatalf("invocation = %q, %q", command, arguments)
	}
}

func TestParseInvocation(t *testing.T) {
	var storage [MaxArguments][MaxArgumentSize]byte
	invocation, ok := ParseInvocation([]byte(`echo "hello world" 'two words' escaped\ value`), &storage)
	if !ok || invocation.Count != 4 || string(invocation.Arguments[0]) != "echo" || string(invocation.Arguments[1]) != "hello world" || string(invocation.Arguments[2]) != "two words" || string(invocation.Arguments[3]) != "escaped value" {
		t.Fatalf("invocation = %#v, %v", invocation, ok)
	}
	if _, ok := ParseInvocation([]byte(`echo "unterminated`), &storage); ok {
		t.Fatal("unterminated quote accepted")
	}
}

func TestSplitPipeline(t *testing.T) {
	left, right, ok := SplitPipeline([]byte(`echo "a|b" | readline`))
	if !ok || string(left) != `echo "a|b"` || string(right) != "readline" {
		t.Fatalf("pipeline = %q | %q, %v", left, right, ok)
	}
	for _, input := range []string{"| readline", "echo x |", "a | b | c", `echo "unterminated | b`} {
		if _, _, ok := SplitPipeline([]byte(input)); ok {
			t.Fatalf("accepted %q", input)
		}
	}
}
