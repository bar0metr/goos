package admincmd

import "testing"

type args []string

func (arguments args) ArgCount() int { return len(arguments) }
func (arguments args) Arg(index int) ([]byte, bool) {
	if index < 1 || index > len(arguments) {
		return nil, false
	}
	return []byte(arguments[index-1]), true
}

func TestNativeArgumentGrammar(t *testing.T) {
	for _, test := range []struct {
		args  args
		count uint64
		file  int
		ok    bool
	}{
		{args{"file"}, 10, 1, true}, {args{"-n", "0", "file"}, 0, 3, true}, {args{"-n", "18446744073709551615", "file"}, ^uint64(0), 3, true},
		{args{}, 10, 1, false}, {args{"file", "extra"}, 10, 1, false}, {args{"-n", "1"}, 0, 0, false}, {args{"-n", "18446744073709551616", "file"}, 0, 3, false},
	} {
		count, file, ok := LinesArguments(test.args)
		if count != test.count || file != test.file || ok != test.ok {
			t.Fatalf("lines %v = %d %d %v", test.args, count, file, ok)
		}
	}
	for _, test := range []struct {
		args    args
		pattern string
		fold    bool
		file    int
		ok      bool
	}{
		{args{"x"}, "x", false, 0, true}, {args{"x", "file"}, "x", false, 2, true}, {args{"-i", "x"}, "x", true, 0, true}, {args{"-i", "x", "file"}, "x", true, 3, true},
		{args{""}, "", false, 0, true}, {args{}, "", false, 0, false}, {args{"-i"}, "", true, 0, false}, {args{"x", "a", "b"}, "x", false, 0, false},
	} {
		pattern, fold, file, ok := GrepArguments(test.args)
		if string(pattern) != test.pattern || fold != test.fold || file != test.file || ok != test.ok {
			t.Fatalf("grep %v = %q %v %d %v", test.args, pattern, fold, file, ok)
		}
	}
	for _, test := range []struct {
		args args
		mode byte
		file int
		ok   bool
	}{
		{args{}, 0, 0, true}, {args{"file"}, 0, 1, true}, {args{"-l"}, 'l', 0, true}, {args{"-w", "file"}, 'w', 2, true}, {args{"-c", "file"}, 'c', 2, true},
		{args{"-x"}, 0, 0, false}, {args{"-lc"}, 0, 0, false}, {args{"a", "b"}, 0, 0, false},
	} {
		mode, file, ok := WCArguments(test.args)
		if mode != test.mode || file != test.file || ok != test.ok {
			t.Fatalf("wc %v = %c %d %v", test.args, mode, file, ok)
		}
	}
	for _, arguments := range []args{{"/"}, {"/", "-name", "file"}} {
		if _, ok := FindArguments(arguments); !ok {
			t.Fatal(arguments)
		}
	}
	for _, arguments := range []args{{}, {"/", "-name"}, {"/", "-name", ""}, {"/", "-name", "a/b"}, {"/", "-name", "a\x00b"}, {"/", "-iname", "a"}} {
		if _, ok := FindArguments(arguments); ok {
			t.Fatal(arguments)
		}
	}
	for _, arguments := range []args{{}, {"-u"}} {
		if !DateArguments(arguments) {
			t.Fatal(arguments)
		}
	}
	for _, arguments := range []args{{"-l"}, {"-u", "-u"}} {
		if DateArguments(arguments) {
			t.Fatal(arguments)
		}
	}
}
