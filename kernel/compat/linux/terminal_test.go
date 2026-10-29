package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestTermiosWireLayout(t *testing.T) {
	if abi.TermiosSize != 36 || abi.TermiosCharsOffset+abi.TermiosControlChars != abi.TermiosSize || abi.WinsizeSize != 8 {
		t.Fatal("struct termios or winsize has the wrong size")
	}
	if abi.IoctlTCGETS != 0x5401 || abi.IoctlTIOCGWINSZ != 0x5413 || abi.IoctlTIOCSPGRP != 0x5410 {
		t.Fatal("ioctl numbers differ from Linux")
	}
	settings := DefaultTermios()
	var wire [abi.TermiosSize]byte
	settings.Encode(&wire)
	// The values a Linux PTY reports at start, from the reference trace,
	// with IXON clear and IUTF8 set.
	if wire[0] != 0x00 || wire[1] != 0x41 || wire[4] != 0x05 || wire[8] != 0xbf || wire[12] != 0x3b || wire[13] != 0x8a {
		t.Fatalf("encoded defaults % x", wire[:16])
	}
	if wire[abi.TermiosCharsOffset+abi.VINTR] != 3 || wire[abi.TermiosCharsOffset+abi.VMIN] != 1 {
		t.Fatal("control characters misplaced")
	}
	if DecodeTermios(&wire) != settings {
		t.Fatal("termios does not round trip")
	}
}

func TestCanonicalLineEditing(t *testing.T) {
	var discipline Discipline
	discipline.Reset()
	var echo Echo
	discipline.Feed([]byte("helo\x7flo wrd\x17world\r"), &echo)
	var line [64]byte
	if !discipline.Readable(64) {
		t.Fatal("a completed line is not readable")
	}
	count := discipline.Read(line[:])
	if string(line[:count]) != "hello world\n" {
		t.Fatalf("line = %q", line[:count])
	}
	if string(echo.Bytes()) != "helo\b \blo wrd\b \b\b \b\b \bworld\n" {
		t.Fatalf("echo = %q", echo.Bytes())
	}
	// End of file on an empty line is a read of nothing, once.
	discipline.Feed([]byte{0x04}, &echo)
	if !discipline.Readable(1) || discipline.Read(line[:]) != 0 || discipline.Readable(1) {
		t.Fatal("end of file is not one empty read")
	}
	// A line ended by end of file goes without the character.
	discipline.Feed([]byte("ab\x04"), &echo)
	if count := discipline.Read(line[:]); string(line[:count]) != "ab" {
		t.Fatalf("eof line = %q", line[:count])
	}
	// A partial line waits; the kill character discards it.
	discipline.Feed([]byte("junk\x15ok\n"), &echo)
	if count := discipline.Read(line[:]); string(line[:count]) != "ok\n" {
		t.Fatalf("after kill = %q", line[:count])
	}
}

func TestSignalCharactersFlushAndSignal(t *testing.T) {
	var discipline Discipline
	discipline.Reset()
	var echo Echo
	signals := discipline.Feed([]byte("typed\x03"), &echo)
	if !signals.Has(abi.SignalInterrupt) || discipline.Pending() != 0 || discipline.lineSize != 0 {
		t.Fatal("^C did not signal and flush")
	}
	if string(echo.Bytes()) != "typed^C" {
		t.Fatalf("echo = %q", echo.Bytes())
	}
	if signals := discipline.Feed([]byte{0x1a, 0x1c}, &echo); !signals.Has(abi.SignalTermStop) || !signals.Has(abi.SignalQuit) {
		t.Fatal("^Z and ^\\ did not signal")
	}
	settings := discipline.Settings
	settings.Local &^= abi.ISIG
	discipline.Apply(settings)
	if signals := discipline.Feed([]byte{0x03}, &echo); signals != 0 {
		t.Fatal("^C signalled without ISIG")
	}
}

func TestNonCanonicalReadsWhatArrived(t *testing.T) {
	var discipline Discipline
	discipline.Reset()
	var echo Echo
	discipline.Feed([]byte("par"), &echo)
	// Leaving canonical mode makes the partial line readable, as nano sees.
	settings := discipline.Settings
	settings.Local &^= abi.ICANON | abi.ECHO | abi.ISIG
	settings.Input = 0
	discipline.Apply(settings)
	echo.Reset()
	discipline.Feed([]byte("\x1b[A\r\x03"), &echo)
	if len(echo.Bytes()) != 0 {
		t.Fatalf("echo without ECHO: %q", echo.Bytes())
	}
	var buffer [2]byte
	var all []byte
	for discipline.Readable(len(buffer)) {
		count := discipline.Read(buffer[:])
		all = append(all, buffer[:count]...)
	}
	if string(all) != "par\x1b[A\r\x03" {
		t.Fatalf("raw input = %q", all)
	}
	settings.Chars[abi.VMIN] = 0
	discipline.Apply(settings)
	if !discipline.Readable(1) || discipline.Read(buffer[:]) != 0 {
		t.Fatal("VMIN 0 does not return at once")
	}
}

func TestOutputProcessing(t *testing.T) {
	settings := DefaultTermios()
	source := []byte("a\nb\x1b[7m\n")
	var target [32]byte
	consumed, produced := settings.OutputSize(source, len(target))
	if consumed != len(source) || produced != len(source)+2 {
		t.Fatalf("size %d %d", consumed, produced)
	}
	if count := settings.TransformOutput(source, target[:]); string(target[:count]) != "a\r\nb\x1b[7m\r\n" {
		t.Fatalf("output %q", target[:count])
	}
	// A newline that would not fit whole waits for the next chunk.
	if consumed, produced := settings.OutputSize([]byte("ab\n"), 3); consumed != 2 || produced != 2 {
		t.Fatalf("partial %d %d", consumed, produced)
	}
	settings.Output = 0
	if count := settings.TransformOutput(source, target[:]); string(target[:count]) != string(source) {
		t.Fatal("output changed without OPOST")
	}
}

func TestTerminalGenerationAndReferences(t *testing.T) {
	resetCompatibilityProcesses()
	endpoints := TerminalEndpoints{Input: 1, Output: 2, Error: 3, Control: 4}
	id, ok := CreateTerminal(7, endpoints, 24, 80, 5)
	if !ok || !RetainTerminal(id) {
		t.Fatal("create or retain refused")
	}
	if _, last := ReleaseTerminal(id); last {
		t.Fatal("first release was the last")
	}
	if released, last := ReleaseTerminal(id); !last || released != endpoints {
		t.Fatal("last release did not return the endpoints")
	}
	again, _ := CreateTerminal(7, endpoints, 24, 80, 5)
	if again == id {
		t.Fatal("a reused slot kept its identity")
	}
	if _, found := Terminal(id); found || RetainTerminal(id) {
		t.Fatal("a stale identity names the new terminal")
	}
	if changed, snapshot := ResizeTerminal(again, 50, 132); !changed || snapshot.Rows != 50 || snapshot.Foreground != 5 {
		t.Fatal("resize not recorded")
	}
	if changed, _ := ResizeTerminal(again, 50, 132); changed {
		t.Fatal("the same size counted as a change")
	}
}

func TestSessionsAndGroups(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(400, 1, 0)
	child, _ := registerChildForTest(401, root.PID)
	other, _ := installProcessRecord(402, 1, 0)
	if root.SID != root.PID || root.PGID != root.PID || child.SID != root.PID || child.PGID != root.PID {
		t.Fatal("identities not inherited")
	}
	if _, errno := Setsid(root.PID); errno != abi.EPERM {
		t.Fatal("a group leader made a session")
	}
	if Setpgid(root.PID, 0, 0) != abi.EPERM {
		t.Fatal("a session leader changed group")
	}
	if Setpgid(root.PID, child.PID, child.PID) != 0 {
		t.Fatal("parent could not move its child")
	}
	if group, _ := ProcessGroupOf(root.PID, child.PID); group != child.PID {
		t.Fatal("child group not changed")
	}
	if Setpgid(root.PID, child.PID, 30000) != abi.EPERM {
		t.Fatal("moved into a group that does not exist")
	}
	if _, errno := ProcessGroupOf(root.PID, other.PID); errno != abi.ESRCH {
		t.Fatal("another family's process was visible")
	}
	if GroupOrphaned(root.FamilyID, child.PID) || !GroupOrphaned(root.FamilyID, root.PID) {
		t.Fatal("orphaned groups misjudged")
	}
	MarkExecuted(child.PID)
	if Setpgid(root.PID, child.PID, root.PID) != abi.EACCES {
		t.Fatal("moved a child that replaced its image")
	}
	if sid, errno := Setsid(child.PID); errno != 0 || sid != child.PID {
		// The child leads its group, so setsid is EPERM.
		if errno != abi.EPERM {
			t.Fatalf("setsid of a group leader = %d %d", sid, errno)
		}
	}
	id, _ := CreateTerminal(root.FamilyID, TerminalEndpoints{1, 2, 3, 4}, 24, 80, root.PID)
	if SetTerminalForeground(id, root.PID, child.PID) != 0 {
		t.Fatal("foreground to a group of the session refused")
	}
	if SetTerminalForeground(id, root.PID, 30000) != abi.ESRCH {
		t.Fatal("foreground to no group")
	}
	if SetTerminalForeground(id, other.PID, root.PID) != abi.ENOTTY {
		t.Fatal("a process outside the session set the foreground")
	}
	if controlling, found := ControllingTerminal(root.PID); !found || controlling != id {
		t.Fatal("controlling terminal not found")
	}
	var released Released
	tablesLock.Lock()
	detachSessionHeld(root.PID, &released)
	tablesLock.Unlock()
	if _, found := ControllingTerminal(root.PID); found || released.Len() != 1 || released.At(0).Kind != FDTerminal {
		t.Fatal("session leader exit did not detach the terminal")
	}
}

func TestStopSignalsAndContinue(t *testing.T) {
	var state SignalState
	if !DeliverableSignal(abi.SignalTermStop) || !state.StopsNow(abi.SignalTermStop) || !state.StopsNow(abi.SignalStop) {
		t.Fatal("stop signals do not stop by default")
	}
	if state.Outcome(abi.SignalWinch) != SignalDiscard || state.Outcome(abi.SignalContinue) != SignalDiscard {
		t.Fatal("SIGWINCH and SIGCONT are not ignored by default")
	}
	state.Mask(abi.SignalBlock, 1<<(abi.SignalTermStop-1), true)
	if state.StopsNow(abi.SignalTermStop) || !state.StopsNow(abi.SignalStop) {
		t.Fatal("a blocked SIGTSTP stopped, or SIGSTOP was blockable")
	}
	state.Raise(abi.SignalTermStop, SignalInfo{})
	state.Raise(abi.SignalContinue, SignalInfo{})
	if state.Pending()&(1<<(abi.SignalTermStop-1)) != 0 {
		t.Fatal("SIGCONT left a stop pending")
	}
}
