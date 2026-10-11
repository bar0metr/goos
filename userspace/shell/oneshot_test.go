package shell

import (
	"bytes"
	"goos/userspace/startup"
	"testing"
)

func TestOneShotValidatesBeforeExecution(t *testing.T) {
	var storage Storage
	for _, line := range []string{"echo safe; sleep 1 &", "echo safe; echo a&b", "echo safe; echo x && echo y", "echo a;;echo b", "echo 'unterminated", "echo a|echo b|echo c|echo d|echo e", ""} {
		if _, count, valid := ParseOneShot([]byte(line), &storage); valid || count != 0 {
			t.Fatalf("accepted %q count %d", line, count)
		}
	}
	for _, line := range []string{"echo '&'", `echo "a&b"`, `echo \&`, "echo a; echo b;", "echo a|wc", "time echo a; echo b", "time echo ok > /tmp/file", "time echo ok | wc", "time time echo ok | wc"} {
		if _, _, valid := ParseOneShot([]byte(line), &storage); !valid {
			t.Fatalf("rejected %q", line)
		}
	}
	for _, n := range []int{MaxLine, MaxLine + 1} {
		_, _, valid := ParseOneShot(bytes.Repeat([]byte("a"), n), &storage)
		if valid != (n == MaxLine) {
			t.Fatalf("bound %d: %v", n, valid)
		}
	}
}

func TestOneShotStartupWithoutTerminal(t *testing.T) {
	var buffer [startup.MaxSize]byte
	writer := startup.NewWriter(buffer[:], 3, 0, 3)
	writer.Command([]byte("shell"))
	writer.CWD([]byte("/"))
	writer.Argument([]byte("shell"))
	writer.Argument([]byte("-c"))
	writer.Argument([]byte("echo ok"))
	writer.Handle([]byte("stdin"), 1)
	writer.Handle([]byte("stdout"), 2)
	writer.Handle([]byte("stderr"), 3)
	encoded, valid := writer.Finish()
	if !valid {
		t.Fatal("startup encode")
	}
	data, valid := startup.Decode(encoded)
	if !valid {
		t.Fatal("startup decode")
	}
	command, mode, valid := OneShotCommand(data)
	if !valid || !mode || string(command) != "echo ok" {
		t.Fatalf("mode=%v valid=%v command=%q", mode, valid, command)
	}
	if _, found := data.Handle("tty"); found {
		t.Fatal("unexpected tty")
	}
}
