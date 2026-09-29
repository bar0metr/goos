package filemanager

import (
	"strings"
	"testing"

	supervisor "goos/userspace/init"
	"goos/userspace/terminal"
)

func launchHarness(t *testing.T, columns, rows int) (*harness, *programs) {
	t.Helper()
	filesystem := newMemoryFS("/d/", "/d/sub/", "/d/a.txt", "/d/b.txt")
	h := newHarness(t, filesystem, "/d", columns, rows)
	runner := &programs{}
	h.app.SetPrograms(runner)
	return h, runner
}

// selectName moves the active panel's selection to name.
func (h *harness) selectName(name string) {
	h.keys(terminal.KeyHome)
	for h.selected() != name {
		before := h.app.Active().Selected()
		h.keys(terminal.KeyDown)
		if h.app.Active().Selected() == before {
			h.t.Fatalf("%q is not listed", name)
		}
	}
}

func TestViewerAndEditorRunOnTheSelectedFile(t *testing.T) {
	h, runner := launchHarness(t, 80, 24)
	h.selectName("a.txt")
	h.app.Handle(terminal.Event{Key: terminal.KeyF3})
	if h.app.screen.cleared {
		t.Fatal("the screen was not given up for a full repaint")
	}
	h.render()
	h.keys(terminal.KeyEnter, terminal.KeyF4)
	want := []string{"gview /d/a.txt in /d", "gview /d/a.txt in /d", "edit /d/a.txt in /d"}
	if strings.Join(runner.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("started %q", runner.calls)
	}
	if h.status() != "" || h.selected() != "a.txt" {
		t.Fatalf("after return: status %q, selected %q", h.status(), h.selected())
	}
	if h.app.Launches != 3 {
		t.Fatalf("launches counted %d", h.app.Launches)
	}
}

func TestDirectoriesAreNotOpenedAsFiles(t *testing.T) {
	h, runner := launchHarness(t, 80, 24)
	h.selectName("sub")
	h.keys(terminal.KeyF3)
	if len(runner.calls) != 0 || h.status() != "Select a file to open with gview" {
		t.Fatalf("F3 on a directory: %q %q", runner.calls, h.status())
	}
	h.selectName("..")
	h.keys(terminal.KeyF4)
	if len(runner.calls) != 0 || h.status() != "Select a file to open with edit" {
		t.Fatalf("F4 on ..: %q %q", runner.calls, h.status())
	}
}

func TestLaunchFailuresAreMessages(t *testing.T) {
	h, runner := launchHarness(t, 80, 24)
	h.selectName("a.txt")
	runner.status = supervisor.LaunchNotFound
	h.keys(terminal.KeyF4)
	if h.status() != "Cannot start edit: "+supervisor.LaunchNotFound.Reason() || h.app.Quit() {
		t.Fatalf("missing editor: %q", h.status())
	}
	runner.status, runner.code = supervisor.LaunchOK, 3
	h.keys(terminal.KeyF3)
	if h.status() != "gview ended with status 3" {
		t.Fatalf("crashed viewer: %q", h.status())
	}
	h.keys(terminal.KeyDown)
	if h.app.Quit() || h.status() != "" {
		t.Fatal("the manager did not carry on after a failed program")
	}
}

// TestReturnFromEditorRefreshesAndKeepsTheSelection has the editor grow the
// file and create another that sorts before it: the listing is read again,
// the size is new, and the same file stays selected.
func TestReturnFromEditorRefreshesAndKeepsTheSelection(t *testing.T) {
	h, runner := launchHarness(t, 80, 24)
	h.selectName("b.txt")
	calls := h.fs.infoCalls
	runner.run = func() {
		h.fs.files["/d/b.txt"] = []byte(strings.Repeat("x", 12345))
		h.fs.files["/d/0first.txt"] = []byte("new")
	}
	h.keys(terminal.KeyF4)
	if h.selected() != "b.txt" {
		t.Fatalf("selection moved to %q", h.selected())
	}
	if entry, _, _ := h.app.Active().Current(); entry.Size != 12345 {
		t.Fatalf("size after edit %d", entry.Size)
	}
	if h.fs.infoCalls == calls {
		t.Fatal("free space was not asked for again after the editor")
	}
}

func TestMovingTheSelectionDoesNotAskForCapacity(t *testing.T) {
	h, _ := launchHarness(t, 80, 24)
	calls := h.fs.infoCalls
	h.keys(terminal.KeyDown, terminal.KeyDown, terminal.KeyUp, terminal.KeyPageDown, terminal.KeyTab)
	if h.fs.infoCalls != calls {
		t.Fatalf("cursor keys asked for capacity %d times", h.fs.infoCalls-calls)
	}
	h.keys(terminal.KeyF2)
	if h.fs.infoCalls != calls+2 {
		t.Fatalf("F2 asked %d times, want 2", h.fs.infoCalls-calls)
	}
}

func TestCapacityIsShownOnTheBottomFrame(t *testing.T) {
	h, _ := launchHarness(t, 80, 24)
	if bottom := h.screen.line(21); !strings.Contains(bottom, "18.3G free") {
		t.Fatalf("80 columns: %q", bottom)
	}
	wide, _ := launchHarness(t, 106, 33)
	if bottom := wide.screen.line(30); !strings.Contains(bottom, "fat32 18.3G free / 32.0G") {
		t.Fatalf("106 columns: %q", bottom)
	}
	readOnly := newHarness(t, newMemoryFS("/ro/", "/ro/x"), "/ro", 80, 24)
	if bottom := readOnly.screen.line(21); !strings.Contains(bottom, "image ro") {
		t.Fatalf("read-only: %q", bottom)
	}
}

func TestModificationTimeFollowsTheWidth(t *testing.T) {
	narrow, _ := launchHarness(t, 80, 24)
	narrow.selectName("a.txt")
	if strings.Contains(narrow.screen.line(1), "Modified") {
		t.Fatal("80 columns grew a date column")
	}
	if info := narrow.screen.line(20); !strings.Contains(info, "29 Sep 11:42") {
		t.Fatalf("information line: %q", info)
	}
	wide, _ := launchHarness(t, 106, 33)
	if header := wide.screen.line(1); !strings.Contains(header, "Modified") {
		t.Fatalf("106 columns header: %q", header)
	}
	if !strings.Contains(wide.screen.text(), "a.txt") || !strings.Contains(wide.screen.line(5), "29 Sep 11:42") {
		t.Fatalf("106 columns:\n%s", wide.screen.text())
	}
	wide.app.SetYear(2025)
	wide.keys(terminal.KeyF2)
	if !strings.Contains(wide.screen.line(5), "29 Sep  2026") {
		t.Fatalf("another year: %q", wide.screen.line(5))
	}
}

func TestRenameInPlaceKeepsTheEntrySelected(t *testing.T) {
	h, _ := launchHarness(t, 80, 24)
	h.selectName("b.txt")
	h.keys(terminal.KeyF6)
	for h.app.dialog.inputSize > 0 {
		h.app.Handle(terminal.Event{Key: terminal.KeyBackspace})
	}
	h.typeText("/d/0renamed.txt")
	h.keys(terminal.KeyEnter)
	if h.selected() != "0renamed.txt" {
		t.Fatalf("selected %q after rename (%q)", h.selected(), h.status())
	}
}
