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
		{`readline < "/tmp/input file" > /tmp/output`, []string{`readline < "/tmp/input file" > /tmp/output`}},
		{strings.Repeat("pwd;", MaxCommands), []string{"pwd", "pwd", "pwd", "pwd", "pwd", "pwd", "pwd", "pwd"}},
	} {
		var scratch Storage
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
		"pwd; a | b | c | d > out", "pwd; a | b | c | d < in", "pwd; a | b | c < in > out",
		"pwd; cd / > out", "pwd; echo > a b",
		"pwd; echo x | help", "pwd; echo x | time readline", "pwd; time echo x > out",
		"pwd; echo " + strings.Repeat("x", MaxLine), strings.Repeat("pwd;", MaxCommands+1),
	} {
		var scratch Storage
		if _, _, ok := ParseSequence([]byte(line), &scratch); ok {
			t.Fatalf("accepted malformed sequence %q", line)
		}
	}
}

func TestSplitTime(t *testing.T) {
	for _, test := range []struct {
		line, command string
		timed         bool
	}{
		{"time", "", true},
		{"  time   ", "", true},
		{"time uname", "uname", true},
		{" time  echo y > /persist/x; pwd ", "echo y > /persist/x; pwd", true},
		{"time time echo", "time echo", true},
		{"timer", "", false},
		{"times x", "", false},
		{"echo time", "", false},
		{"", "", false},
	} {
		command, timed := SplitTime([]byte(test.line))
		if timed != test.timed || string(command) != test.command {
			t.Fatalf("SplitTime(%q) = %q, %v", test.line, command, timed)
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
	var storage Storage
	invocation, ok := ParseInvocation([]byte(`echo "hello world" 'two words' escaped\ value`), &storage)
	if !ok || invocation.Count != 4 || string(invocation.Arguments[0]) != "echo" || string(invocation.Arguments[1]) != "hello world" || string(invocation.Arguments[2]) != "two words" || string(invocation.Arguments[3]) != "escaped value" {
		t.Fatalf("invocation = %#v, %v", invocation, ok)
	}
	if _, ok := ParseInvocation([]byte(`echo "unterminated`), &storage); ok {
		t.Fatal("unterminated quote accepted")
	}
}

// TestParseInvocationIsBoundedByBytes covers what the eight-word, 127-byte
// tokenizer refused: a line is limited by its length alone.
func TestParseInvocationIsBoundedByBytes(t *testing.T) {
	var storage Storage
	words := strings.Repeat(" a", MaxLine/2-2)
	invocation, ok := ParseInvocation([]byte("echo"+words), &storage)
	if !ok || invocation.Count != MaxLine/2-1 || string(invocation.Arguments[invocation.Count-1]) != "a" {
		t.Fatalf("count = %d, %v", invocation.Count, ok)
	}
	long := "echo " + strings.Repeat("x", MaxLine-5)
	invocation, ok = ParseInvocation([]byte(long), &storage)
	if !ok || len(invocation.Arguments[1]) != MaxLine-5 {
		t.Fatalf("a %d-byte line refused", len(long))
	}
	if _, ok := ParseInvocation([]byte(long+"x"), &storage); ok {
		t.Fatal("a line past MaxLine accepted")
	}
	// The densest line fills every word slot.
	dense := "a" + strings.Repeat(" a", MaxLine/2-1)
	if invocation, ok := ParseInvocation([]byte(dense), &storage); !ok || invocation.Count != MaxLine/2 {
		t.Fatalf("dense line: %d words, %v", invocation.Count, ok)
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

func TestSplitRedirections(t *testing.T) {
	for _, test := range []struct {
		line, command, input, output, failure string
		appendMode, failureAppend             bool
	}{
		{line: "echo value", command: "echo value"},
		{line: "echo value < /tmp/in", command: "echo value", input: "/tmp/in"},
		{line: "echo value > /tmp/out", command: "echo value", output: "/tmp/out"},
		{line: "echo value >> /tmp/out", command: "echo value", output: "/tmp/out", appendMode: true},
		{line: "echo value < /tmp/in >> /tmp/out", command: "echo value", input: "/tmp/in", output: "/tmp/out", appendMode: true},
		{line: "echo value > /tmp/out < /tmp/in", command: "echo value", input: "/tmp/in", output: "/tmp/out"},
		{line: `echo value < "/tmp/input file"`, command: "echo value", input: `"/tmp/input file"`},
		{line: `echo "a>b<c"`, command: `echo "a>b<c"`},
		{line: `echo a\>b\<c`, command: `echo a\>b\<c`},
		{line: "echo value 2> /tmp/err", command: "echo value", failure: "/tmp/err"},
		{line: "echo value 2>> /tmp/err", command: "echo value", failure: "/tmp/err", failureAppend: true},
		{line: "echo value 2> /tmp/err > /tmp/out", command: "echo value", output: "/tmp/out", failure: "/tmp/err"},
		{line: "echo value >> /tmp/out 2>> /tmp/err < /tmp/in", command: "echo value", input: "/tmp/in", output: "/tmp/out", appendMode: true, failure: "/tmp/err", failureAppend: true},
		{line: "echo value2> /tmp/out", command: "echo value2", output: "/tmp/out"},
		{line: "echo 2 > /tmp/out", command: "echo 2", output: "/tmp/out"},
		{line: `echo "x 2> y"`, command: `echo "x 2> y"`},
		{line: `echo x 2\> y`, command: `echo x 2\> y`},
	} {
		redirects, ok := SplitRedirections([]byte(test.line))
		if !ok || redirects.Append != test.appendMode || redirects.ErrorAppend != test.failureAppend || string(redirects.Command) != test.command || string(redirects.Input) != test.input || string(redirects.Output) != test.output || string(redirects.Error) != test.failure {
			t.Fatalf("SplitRedirections(%q) = %+v, %v", test.line, redirects, ok)
		}
	}
	for _, input := range []string{"< in", "> out", ">> out", "echo <", "echo >", "echo >>", "echo < a b", "echo > a b", "echo >>> out", "echo < a < b", "echo > a > b", "2> out", "echo 2>", "echo 2>>", "echo 2> a b", "echo 2> a 2> b", "echo 2>>> out", "echo 2> a 2>> b"} {
		if _, ok := SplitRedirections([]byte(input)); ok {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestRedirectionStagesAndSequenceBounds(t *testing.T) {
	redirects, ok := SplitRedirections([]byte("readline < in > out 2> err"))
	if !ok || redirects.Stages() != 3 {
		t.Fatalf("redirections = %+v, %v", redirects, ok)
	}
	var scratch Storage
	for _, test := range []struct {
		line string
		ok   bool
	}{
		{"echo a 2> err", true},
		{"echo a | readline 2> err", true},
		{"echo a | readline | readline 2> err", true},
		{"echo a | readline | readline | readline 2> err", false},
		{"echo a | readline > out 2> err", true},
		{"echo a | readline | readline > out 2> err", false},
		{"readline < in > out 2> err", true},
		{"echo a | readline < in > out 2> err", false},
		{"cd / 2> err", false},
		{"echo a 2> err; echo b 2>> err", true},
	} {
		if _, _, ok := ParseSequence([]byte(test.line), &scratch); ok != test.ok {
			t.Fatalf("ParseSequence(%q) = %v, want %v", test.line, ok, test.ok)
		}
	}
}

func TestSplitBackground(t *testing.T) {
	for _, test := range []struct {
		line, command string
		background    bool
	}{
		{"sleep 2 &", "sleep 2", true},
		{"  cat a | readline&  ", "cat a | readline", true},
		{"echo '&'", "echo '&'", false},
		{"echo \"a &\"", "echo \"a &\"", false},
		{"echo \\&", "echo \\&", false},
		{"echo a & b", "echo a & b", false},
		{"&", "", true},
	} {
		command, background := SplitBackground([]byte(test.line))
		if string(command) != test.command || background != test.background {
			t.Errorf("SplitBackground(%q) = %q, %v", test.line, command, background)
		}
	}
}
