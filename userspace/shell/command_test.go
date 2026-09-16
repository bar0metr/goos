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
