package tty

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/ipc"
)

// console replays what the terminal service writes, so the tests can state
// what a person would see as well as which bytes went out.
type console struct {
	screen  Screen
	line    Discipline
	written string
}

func (c *console) write(data []byte) { c.written += string(data) }

func (c *console) show(prompt string) {
	c.write(c.screen.Hide())
	c.write(c.screen.Show([]byte(prompt), c.line.Pending()))
}

func (c *console) typed(input string) {
	for index := 0; index < len(input); index++ {
		c.line.Feed(input[index])
		c.write(c.line.Echo())
		c.screen.Echoed(c.line.Echo(), len(c.line.Pending()))
	}
}

// output is what serveOutput does with one batch: hide, write, show again.
func (c *console) output(data, prompt string) {
	c.write(c.screen.Hide())
	c.write([]byte(data))
	c.screen.Output([]byte(data))
	c.write(c.screen.Show([]byte(prompt), c.line.Pending()))
}

func TestOutputUnderAPromptRedrawsTheLine(t *testing.T) {
	var c console
	c.show("goos> ")
	c.typed("ec")
	c.written = ""
	c.output("[1] done\n", "goos> ")
	if c.written != "\r\x1b[K[1] done\ngoos> ec" {
		t.Fatalf("written %q", c.written)
	}
	if active, shown := c.screen.Shown(); !active || shown != 8 {
		t.Fatalf("shown %v %d", active, shown)
	}
}

func TestOutputWithNothingShownIsWrittenAsItIs(t *testing.T) {
	var c console
	// A foreground command runs: the read is outstanding with no prompt.
	c.show("")
	c.output("hello\n", "")
	c.output("world\n", "")
	if c.written != "hello\nworld\n" {
		t.Fatalf("written %q", c.written)
	}
}

func TestPromptAfterUnfinishedOutputStartsANewRow(t *testing.T) {
	var c console
	c.show("")
	c.output("Enter name: ", "")
	// Typing answers on the same row.
	c.typed("bo")
	if c.written != "Enter name: bo" {
		t.Fatalf("written %q", c.written)
	}
	// More output erases only the typed columns, not the program's text.
	c.written = ""
	c.output("!", "")
	if c.written != "\x1b[2D\x1b[K!\nbo" {
		t.Fatalf("written %q", c.written)
	}
	c.typed("b\r")
	c.written = ""
	c.show("goos> ")
	if c.written != "goos> " {
		t.Fatalf("prompt after a finished line: %q", c.written)
	}
}

func TestPromptNeverTrailsUnfinishedOutput(t *testing.T) {
	var c console
	c.show("goos> ")
	c.output("partial", "goos> ")
	if c.written != "goos> \r\x1b[Kpartial\ngoos> " {
		t.Fatalf("written %q", c.written)
	}
}

func TestShowingAPromptAgainReplacesIt(t *testing.T) {
	var c console
	c.show("goos> ")
	// End of input at the prompt answers the read; the next read must not
	// print a second prompt beside the first.
	c.show("goos> ")
	if c.written != "goos> \r\x1b[Kgoos> " {
		t.Fatalf("written %q", c.written)
	}
}

func TestEnterAndInterruptEndTheLine(t *testing.T) {
	var c console
	c.show("goos> ")
	c.typed("ls\r")
	if active, _ := c.screen.Shown(); active {
		t.Fatal("a completed line is still on the screen")
	}
	c.line.Next()
	c.written = ""
	c.output("bin/\n", "")
	if c.written != "bin/\n" {
		t.Fatalf("output after Enter %q", c.written)
	}
	c.show("goos> ")
	c.typed("x\x03")
	if active, _ := c.screen.Shown(); active {
		t.Fatal("an interrupted line is still on the screen")
	}
}

func TestClearingForgetsTheLine(t *testing.T) {
	var c console
	c.show("goos> ")
	c.typed("abc")
	c.screen.Cleared()
	if active, _ := c.screen.Shown(); active || len(c.screen.Hide()) != 0 {
		t.Fatal("a cleared screen still has a line to erase")
	}
	if shown := c.screen.Show([]byte("goos> "), c.line.Pending()); string(shown) != "goos> abc" {
		t.Fatalf("redisplay %q", shown)
	}
}

func TestSessionNoticeCarriesOneEndpoint(t *testing.T) {
	message, ok := SessionNotice(handle.ID(0x100000009))
	if !ok {
		t.Fatal("notice not encoded")
	}
	session, valid := DecodeSessionNotice(&message)
	if !valid || session != handle.ID(0x100000009) {
		t.Fatalf("decoded %x %v", session, valid)
	}
	if _, ok := SessionNotice(0); ok {
		t.Fatal("a notice without an endpoint was encoded")
	}
	// A notice is not a request: the service must not treat it as one.
	if _, _, valid := DecodeRequest(&message); valid {
		t.Fatal("a notice decoded as a request")
	}
	for name, mutate := range map[string]func(*ipc.Message){
		"no handle":      func(m *ipc.Message) { m.HandleCount = 0 },
		"two handles":    func(m *ipc.Message) { m.HandleCount = 2; m.Handles[1] = 7 },
		"a request":      func(m *ipc.Message) { m.Kind = ipc.KindRequest; m.Correlation = 1 },
		"wrong protocol": func(m *ipc.Message) { m.ProtocolID++ },
		"wrong version":  func(m *ipc.Message) { m.Version++ },
		"wrong payload":  func(m *ipc.Message) { m.Payload[0] = byte(OperationReadLine) },
	} {
		forged := message
		mutate(&forged)
		if _, valid := DecodeSessionNotice(&forged); valid {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, ok := Request(OperationSession, nil, 1); ok {
		t.Fatal("a session change was encoded as a client request")
	}
}
