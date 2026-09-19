package shell

import (
	"strings"
	"testing"
)

func TestParseSequence(t *testing.T) {
	for _, test := range []struct {
		line string
		want []string
	}{
		{"", nil}, {"   ", nil},
		{"echo a; pwd;", []string{"echo a", "pwd"}},
		{`echo "a;b"; echo 'c;d'; echo e\;f`, []string{`echo "a;b"`, `echo 'c;d'`, `echo e\;f`}},
		{`echo x | readline; cd "/system"; pwd`, []string{`echo x | readline`, `cd "/system"`, "pwd"}},
		{`echo redirected > "/tmp/result file"; cat "/tmp/result file"`, []string{`echo redirected > "/tmp/result file"`, `cat "/tmp/result file"`}},
		{strings.Repeat("pwd;", MaxCommands), []string{"pwd", "pwd", "pwd", "pwd", "pwd", "pwd", "pwd", "pwd"}},
	} {
		var scratch [MaxArguments][MaxArgumentSize]byte
		commands, count, ok := ParseSequence([]byte(test.line), &scratch)
		if !ok || count != len(test.want) {
			t.Fatalf("ParseSequence(%q): count=%d ok=%v", test.line, count, ok)
		}
		for i, want := range test.want {
			if string(commands[i]) != want {
				t.Fatalf("command %d = %q, want %q", i, commands[i], want)
			}
		}
	}
}

func TestParseSequenceRejectsMalformedTail(t *testing.T) {
	for _, line := range []string{
		";pwd", "pwd;;pwd", "pwd; ;", `pwd; echo "unfinished`, "pwd; echo x\\",
		"pwd; echo x |", "pwd; a | b | c | d | e", "pwd; cd / | readline",
		"pwd; a | b | c | d > out", "pwd; cd / > out", "pwd; echo > a b",
		"pwd; echo x | help", "pwd; echo " + strings.Repeat("x", MaxArgumentSize+1),
		"pwd; echo a b c d e f g h", strings.Repeat("pwd;", MaxCommands+1),
	} {
		var scratch [MaxArguments][MaxArgumentSize]byte
		if _, _, ok := ParseSequence([]byte(line), &scratch); ok {
			t.Fatalf("accepted malformed sequence %q", line)
		}
	}
}

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
	for _, test := range []struct {
		line string
		want []string
	}{
		{`echo "a|b" | readline`, []string{`echo "a|b"`, "readline"}},
		{`echo a\|b | readline | readline | readline`, []string{`echo a\|b`, "readline", "readline", "readline"}},
		{`echo 'a|b'`, []string{`echo 'a|b'`}},
	} {
		stages, count, ok := SplitPipeline([]byte(test.line))
		if !ok || count != len(test.want) {
			t.Fatalf("pipeline %q count=%d ok=%v", test.line, count, ok)
		}
		for index, want := range test.want {
			if string(stages[index]) != want {
				t.Fatalf("stage = %q, want %q", stages[index], want)
			}
		}
	}
	for _, input := range []string{"", "| readline", "echo x |", "a || b", "a | b | c | d | e", `echo "unterminated | b`, "echo x\\"} {
		if _, _, ok := SplitPipeline([]byte(input)); ok {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestSplitOutputRedirect(t *testing.T) {
	for _, test := range []struct {
		line, command, target string
		redirected            bool
	}{
		{"echo value", "echo value", "", false},
		{"echo value > /tmp/out", "echo value", "/tmp/out", true},
		{`echo value > "/tmp/two words"`, "echo value", `"/tmp/two words"`, true},
		{`echo "a>b"`, `echo "a>b"`, "", false},
		{`echo a\>b`, `echo a\>b`, "", false},
	} {
		command, target, redirected, ok := SplitOutputRedirect([]byte(test.line))
		if !ok || redirected != test.redirected || string(command) != test.command || string(target) != test.target {
			t.Fatalf("SplitOutputRedirect(%q) = %q, %q, %v, %v", test.line, command, target, redirected, ok)
		}
	}
	for _, input := range []string{"> out", "echo >", "echo > a b", "echo >> out", "echo > a > b", "cat < in", "echo error 2> out"} {
		if _, _, _, ok := SplitOutputRedirect([]byte(input)); ok {
			t.Fatalf("accepted %q", input)
		}
	}
}
