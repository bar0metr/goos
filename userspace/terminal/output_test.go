package terminal

import "testing"

func collect(size int) (*Output, *[]byte) {
	var written []byte
	output := NewOutput(make([]byte, size), func(value []byte) { written = append(written, value...) })
	return &output, &written
}

func TestOutputSequences(t *testing.T) {
	output, written := collect(64)
	output.MoveTo(0, 0)
	output.MoveTo(32, 105)
	output.EraseLine()
	output.EraseToEnd()
	output.SetStyle(BrightYellow, Blue)
	output.SetForeground(Default)
	output.SetBackground(BrightBlack)
	output.ResetStyle()
	output.RequestSize()
	output.Flush()
	want := "\x1b[1;1H\x1b[33;106H\x1b[2K\x1b[K\x1b[93;44m\x1b[39m\x1b[100m\x1b[0m\x1b[999;999H\x1b[6n"
	if string(*written) != want {
		t.Fatalf("output = %q, want %q", *written, want)
	}
}

func TestOutputColourCodes(t *testing.T) {
	for color := Black; color <= White; color++ {
		if ForegroundCode(color) != 30+int(color) || BackgroundCode(color) != 40+int(color) {
			t.Fatalf("colour %d", color)
		}
		if ForegroundCode(color+8) != 90+int(color) || BackgroundCode(color+8) != 100+int(color) {
			t.Fatalf("bright colour %d", color)
		}
	}
	if ForegroundCode(Default) != 39 || BackgroundCode(Default) != 49 || ForegroundCode(Color(42)) != 39 {
		t.Fatal("default colour codes")
	}
}

func TestOutputFlushesWhenFull(t *testing.T) {
	output, written := collect(4)
	output.WriteString("abcdefghij")
	if string(*written) != "abcdefgh" {
		t.Fatalf("flushed %q before the end", *written)
	}
	output.Flush()
	if string(*written) != "abcdefghij" {
		t.Fatalf("output = %q", *written)
	}
}

func TestOutputTextFitsAndNeutralisesControls(t *testing.T) {
	output, written := collect(128)
	if used := output.Text([]byte("Привет.txt"), 6); used != 6 {
		t.Fatalf("used %d", used)
	}
	output.Text([]byte("a\x1b[2Jb"), 8)
	output.TextString("─┐", 3)
	output.Flush()
	if string(*written) != "Привет"+"a?[2Jb  "+"─┐ " {
		t.Fatalf("text = %q", *written)
	}
}

func TestWidthAndCut(t *testing.T) {
	if Width([]byte("Ёлка")) != 4 || Width([]byte("ab\xffc")) != 4 || Width(nil) != 0 {
		t.Fatal("width")
	}
	if string(Cut([]byte("Ёлка"), 2)) != "Ёл" || string(Cut([]byte("abc"), 9)) != "abc" {
		t.Fatal("cut")
	}
}

func TestOutputAllocatesNothing(t *testing.T) {
	storage := make([]byte, 256)
	output := NewOutput(storage, func([]byte) {})
	name := []byte("Документ.txt")
	allocations := testing.AllocsPerRun(100, func() {
		output.MoveTo(3, 4)
		output.SetStyle(BrightWhite, Blue)
		output.Text(name, 20)
		output.TextString("F10", 4)
		output.Flush()
	})
	if allocations != 0 {
		t.Fatalf("output allocated %.1f objects", allocations)
	}
}
