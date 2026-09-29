package filemanager

import (
	"fmt"
	"strings"
	"testing"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/terminal"
)

func names(panel *Panel) []string {
	var listed []string
	for index := 0; index < panel.Count(); index++ {
		_, name := panel.Entry(index)
		listed = append(listed, string(name))
	}
	return listed
}

func TestSortingParentFirstDirectoriesBeforeFiles(t *testing.T) {
	filesystem := newMemoryFS("/w/", "/w/b.txt", "/w/A.txt", "/w/zdir/", "/w/Bdir/", "/w/a.txt", "/w/ёж", "/w/Жук", "/w/_x")
	h := newHarness(t, filesystem, "/w", 80, 24)
	want := []string{"..", "Bdir", "zdir", "_x", "A.txt", "a.txt", "b.txt", "Жук", "ёж"}
	if got := names(h.app.Active()); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRootHasNoParentEntryAndParentIsSafe(t *testing.T) {
	h := newHarness(t, newMemoryFS("/tmp/", "/system/"), "/", 80, 24)
	if got := names(h.app.Active()); fmt.Sprint(got) != "[system tmp]" {
		t.Fatalf("root = %v", got)
	}
	h.keys(terminal.KeyBackspace, terminal.KeyBackspace)
	if string(h.app.Active().Path()) != "/" {
		t.Fatal("parent of the root moved")
	}
}

func TestEmptyAndOneEntryDirectories(t *testing.T) {
	h := newHarness(t, newMemoryFS("/empty/", "/one/", "/one/file"), "/empty", 80, 24)
	if got := names(h.app.Active()); fmt.Sprint(got) != "[..]" {
		t.Fatalf("empty = %v", got)
	}
	h.keys(terminal.KeyDown, terminal.KeyPageDown, terminal.KeyEnd, terminal.KeyF5, terminal.KeyF8)
	if h.app.dialog.kind != dialogNone || len(h.fs.calls) != 0 {
		t.Fatalf("operation on '..': dialog %d calls %v", h.app.dialog.kind, h.fs.calls)
	}
	h.keys(terminal.KeyBackspace)
	if h.selected() != "empty" {
		t.Fatalf("returning selected %q, want the directory left", h.selected())
	}
	h.keys(terminal.KeyDown, terminal.KeyEnter)
	if string(h.app.Active().Path()) != "/one" || fmt.Sprint(names(h.app.Active())) != "[.. file]" {
		t.Fatalf("one = %s %v", h.app.Active().Path(), names(h.app.Active()))
	}
}

func TestSelectionMovementAndScrolling(t *testing.T) {
	paths := []string{"/big/"}
	for index := 0; index < 60; index++ {
		paths = append(paths, fmt.Sprintf("/big/f%02d", index))
	}
	h := newHarness(t, newMemoryFS(paths...), "/big", 80, 24)
	panel, visible := h.app.Active(), h.app.listRows()
	if visible != 17 {
		t.Fatalf("list rows = %d", visible)
	}
	h.keys(terminal.KeyDown, terminal.KeyDown)
	if panel.Selected() != 2 || panel.Top() != 0 {
		t.Fatalf("down: %d top %d", panel.Selected(), panel.Top())
	}
	h.keys(terminal.KeyPageDown)
	if panel.Selected() != 2+visible || panel.Top() != 2+visible-visible+1 {
		t.Fatalf("page down: %d top %d", panel.Selected(), panel.Top())
	}
	h.keys(terminal.KeyEnd)
	if panel.Selected() != 60 || panel.Top() != 61-visible {
		t.Fatalf("end: %d top %d", panel.Selected(), panel.Top())
	}
	h.keys(terminal.KeyDown)
	if panel.Selected() != 60 {
		t.Fatal("moved past the end")
	}
	h.keys(terminal.KeyPageUp)
	if panel.Selected() != 60-visible || panel.Top() != 60-visible {
		t.Fatalf("page up: %d top %d", panel.Selected(), panel.Top())
	}
	h.keys(terminal.KeyHome, terminal.KeyUp)
	if panel.Selected() != 0 || panel.Top() != 0 {
		t.Fatalf("home: %d top %d", panel.Selected(), panel.Top())
	}
	if !strings.Contains(h.screen.text(), "60 entries") {
		t.Fatal("entry count is not shown")
	}
}

func TestTabSwitchesPanelsAndNavigationIsIndependent(t *testing.T) {
	h := newHarness(t, newMemoryFS("/a/", "/b/", "/a/x"), "/", 80, 24)
	h.keys(terminal.KeyEnter)
	if string(h.app.Active().Path()) != "/a" || string(h.app.Other().Path()) != "/" {
		t.Fatal("enter changed the wrong panel")
	}
	h.keys(terminal.KeyTab, terminal.KeyDown, terminal.KeyEnter)
	if string(h.app.Active().Path()) != "/b" || string(h.app.Other().Path()) != "/a" {
		t.Fatalf("panels = %s %s", h.app.Active().Path(), h.app.Other().Path())
	}
	if strings.Contains(h.screen.text(), "Left") || strings.Contains(h.screen.text(), "Right") {
		t.Fatal("panels are labelled")
	}
}

func TestRefreshKeepsTheSelection(t *testing.T) {
	filesystem := newMemoryFS("/d/", "/d/b", "/d/c")
	h := newHarness(t, filesystem, "/d", 80, 24)
	h.keys(terminal.KeyDown, terminal.KeyDown)
	filesystem.files["/d/a"] = nil
	h.keys(terminal.KeyF2)
	if h.selected() != "c" || fmt.Sprint(names(h.app.Active())) != "[.. a b c]" {
		t.Fatalf("after refresh %q %v", h.selected(), names(h.app.Active()))
	}
	delete(filesystem.files, "/d/c")
	h.keys(terminal.KeyF2)
	if h.selected() != "b" {
		t.Fatalf("after the selection vanished %q", h.selected())
	}
}

func TestVanishedDirectoryClimbsToAParent(t *testing.T) {
	filesystem := newMemoryFS("/a/", "/a/b/", "/a/b/c/")
	h := newHarness(t, filesystem, "/a/b/c", 80, 24)
	delete(filesystem.directories, "/a/b/c")
	delete(filesystem.directories, "/a/b")
	h.keys(terminal.KeyF2)
	if string(h.app.Active().Path()) != "/a" {
		t.Fatalf("path = %s", h.app.Active().Path())
	}
}

func TestMkdirDialog(t *testing.T) {
	filesystem := newMemoryFS("/tmp/", "/tmp/zz")
	h := newHarness(t, filesystem, "/tmp", 80, 24)
	h.keys(terminal.KeyF7)
	h.typeText("Новая пaпка")
	h.keys(terminal.KeyBackspace)
	if !strings.Contains(h.screen.text(), "Create directory") || !strings.Contains(h.screen.text(), "Новая пaпк_") {
		t.Fatalf("dialog not shown:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyEnter)
	if !filesystem.directories["/tmp/Новая пaпк"] || h.selected() != "Новая пaпк" {
		t.Fatalf("mkdir: %v selected %q", filesystem.calls, h.selected())
	}
	if !strings.Contains(h.screen.text(), "Новая пaпк/") {
		t.Fatalf("new directory not listed:\n%s", h.screen.text())
	}
	// A name with a slash, and Escape, create nothing; a failure is shown.
	h.keys(terminal.KeyF7)
	h.typeText("a/b")
	h.keys(terminal.KeyEnter)
	if h.status() != "Invalid directory name" {
		t.Fatalf("status = %q", h.status())
	}
	h.keys(terminal.KeyF7)
	h.typeText("x")
	h.keys(terminal.KeyEscape)
	h.keys(terminal.KeyF7)
	h.typeText("zz")
	h.keys(terminal.KeyEnter)
	if !strings.HasPrefix(h.status(), "Mkdir failed: already exists") || h.app.Quit() {
		t.Fatalf("status = %q", h.status())
	}
	if len(filesystem.calls) != 2 {
		t.Fatalf("calls = %v", filesystem.calls)
	}
}

func TestCopyToOtherPanelAndOverwriteConfirmation(t *testing.T) {
	filesystem := newMemoryFS("/src/", "/dst/", "/src/file", "/src/dir/", "/dst/file")
	filesystem.files["/src/new"] = []byte("new")
	h := newHarness(t, filesystem, "/src", 80, 24)
	h.keys(terminal.KeyTab, terminal.KeyBackspace, terminal.KeyHome, terminal.KeyEnter, terminal.KeyTab)
	if string(h.app.Other().Path()) != "/dst" {
		t.Fatalf("other = %s", h.app.Other().Path())
	}
	h.keys(terminal.KeyDown, terminal.KeyDown)
	if h.selected() != "file" {
		t.Fatalf("selected %q", h.selected())
	}
	h.keys(terminal.KeyF5)
	if h.app.dialog.kind != dialogConfirm || !strings.Contains(h.screen.text(), "Overwrite \"file\"?") {
		t.Fatalf("no overwrite question:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyEscape)
	if string(filesystem.files["/dst/file"]) != "contents of /dst/file" {
		t.Fatal("cancelled overwrite changed the target")
	}
	h.keys(terminal.KeyF5, terminal.KeyEnter)
	if string(filesystem.files["/dst/file"]) != "contents of /src/file" || h.status() != "Copied \"file\"" {
		t.Fatalf("overwrite: %q %q", filesystem.files["/dst/file"], h.status())
	}
	h.keys(terminal.KeyDown, terminal.KeyF5)
	if string(filesystem.files["/dst/new"]) != "new" || h.app.dialog.kind != dialogNone {
		t.Fatal("copy of a new file asked or failed")
	}
	if fmt.Sprint(names(h.app.Other())) != "[.. file new]" {
		t.Fatalf("target panel not refreshed: %v", names(h.app.Other()))
	}
	h.keys(terminal.KeyHome, terminal.KeyDown, terminal.KeyF5)
	if h.status() != "Directory copy is not supported yet" {
		t.Fatalf("directory copy: %q", h.status())
	}
}

func TestCopyOntoItselfAndErrorsKeepRunning(t *testing.T) {
	filesystem := newMemoryFS("/ro/", "/ro/file", "/w/", "/w/file")
	h := newHarness(t, filesystem, "/w", 80, 24)
	h.keys(terminal.KeyDown, terminal.KeyF5)
	if h.status() != "Cannot copy a file onto itself" {
		t.Fatalf("status = %q", h.status())
	}
	h.keys(terminal.KeyTab, terminal.KeyBackspace, terminal.KeyHome, terminal.KeyEnter, terminal.KeyTab)
	filesystem.files["/w/other"] = []byte("x")
	h.keys(terminal.KeyF2, terminal.KeyEnd, terminal.KeyF5)
	if h.status() != "Copy failed: read-only filesystem" || h.app.Quit() {
		t.Fatalf("status = %q", h.status())
	}
	filesystem.failCopy = vfs.StatusNoSpace
	h.keys(terminal.KeyTab)
	h.keys(terminal.KeyTab)
	delete(filesystem.files, "/w/other")
	h.keys(terminal.KeyF5)
	if !strings.HasPrefix(h.status(), "Copy failed: no such file") {
		t.Fatalf("vanished source: %q", h.status())
	}
	if _, ok := filesystem.files["/w/file"]; !ok {
		t.Fatal("failed copy removed the source")
	}
}

func TestMoveRenameAndCrossFilesystem(t *testing.T) {
	filesystem := newMemoryFS("/a/", "/b/", "/other/", "/a/file", "/a/keep")
	h := newHarness(t, filesystem, "/a", 80, 24)
	h.keys(terminal.KeyTab, terminal.KeyBackspace, terminal.KeyDown, terminal.KeyEnter, terminal.KeyTab)
	h.keys(terminal.KeyDown, terminal.KeyF6)
	if !strings.Contains(h.screen.text(), "/b/file_") {
		t.Fatalf("move target not offered:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyEnter)
	if _, ok := filesystem.files["/b/file"]; !ok || h.status() != "Moved to \"/b/file\"" {
		t.Fatalf("move: %v %q", filesystem.calls, h.status())
	}
	if fmt.Sprint(names(h.app.Active())) != "[.. keep]" || fmt.Sprint(names(h.app.Other())) != "[.. file]" {
		t.Fatal("panels not refreshed after the move")
	}
	// Editing the offered path renames in place.
	h.keys(terminal.KeyDown, terminal.KeyF6)
	for range "/b/keep" {
		h.keys(terminal.KeyBackspace)
	}
	h.typeText("renamed")
	h.keys(terminal.KeyEnter)
	if _, ok := filesystem.files["/a/renamed"]; !ok {
		t.Fatalf("rename: %v", filesystem.calls)
	}
	// A move onto a directory goes into it; across filesystems it is refused
	// and the source stays.
	h.keys(terminal.KeyDown, terminal.KeyF6)
	for range "/b/renamed" {
		h.keys(terminal.KeyBackspace)
	}
	h.typeText("/other")
	h.keys(terminal.KeyEnter)
	if h.status() != "Cross-filesystem move is not supported" {
		t.Fatalf("cross filesystem: %q", h.status())
	}
	if _, ok := filesystem.files["/a/renamed"]; !ok {
		t.Fatal("refused move lost the source")
	}
	if last := filesystem.calls[len(filesystem.calls)-1]; last != "rename /a/renamed /other/renamed" {
		t.Fatalf("last call %q", last)
	}
}

func TestDeleteAsksAndReportsErrors(t *testing.T) {
	filesystem := newMemoryFS("/d/", "/d/full/", "/d/full/x", "/d/file")
	h := newHarness(t, filesystem, "/d", 80, 24)
	h.keys(terminal.KeyEnd, terminal.KeyF8)
	if !strings.Contains(h.screen.text(), "Delete \"file\"?") || !strings.Contains(h.screen.text(), "Enter = yes") {
		t.Fatalf("no question:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyEscape)
	if _, ok := filesystem.files["/d/file"]; !ok {
		t.Fatal("cancelled delete removed the file")
	}
	h.keys(terminal.KeyF8, terminal.KeyEnter)
	if _, ok := filesystem.files["/d/file"]; ok || h.status() != "Deleted \"file\"" {
		t.Fatalf("delete: %q", h.status())
	}
	h.keys(terminal.KeyHome, terminal.KeyDown, terminal.KeyF8, terminal.KeyEnter)
	if h.status() != "Delete failed: directory not empty" || !filesystem.directories["/d/full"] {
		t.Fatalf("non-empty directory: %q", h.status())
	}
}

func TestHelpQuitAndUnavailableKeys(t *testing.T) {
	h := newHarness(t, newMemoryFS("/x"), "/", 80, 24)
	h.keys(terminal.KeyF1)
	if !strings.Contains(h.screen.text(), "gfm keys") || !strings.Contains(h.screen.text(), "F10             quit") {
		t.Fatalf("help:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyF10)
	if h.app.Quit() || strings.Contains(h.screen.text(), "gfm keys") {
		t.Fatal("closing help quit, or left it on the screen")
	}
	h.keys(terminal.KeyF3)
	if !strings.Contains(h.status(), "Cannot start gview: not available") {
		t.Fatalf("F3: %q", h.status())
	}
	h.keys(terminal.KeyDown)
	if h.status() != "" {
		t.Fatal("navigation kept a stale message")
	}
	h.keys(terminal.KeyF10)
	if !h.app.Quit() {
		t.Fatal("F10 did not quit")
	}
	if h.app.FunctionKeys != 4 || h.app.Keys != 5 {
		t.Fatalf("keys counted %d, function keys %d", h.app.Keys, h.app.FunctionKeys)
	}
}

func TestLayoutFitsEveryGeometry(t *testing.T) {
	filesystem := newMemoryFS("/d/", "/d/"+strings.Repeat("длинное-имя-", 30), "/d/"+strings.Repeat("x", 300)+"/")
	filesystem.files["/d/big"] = make([]byte, 0)
	for _, geometry := range [][2]int{{80, 24}, {81, 25}, {106, 33}, {160, 50}, {300, 200}} {
		h := newHarness(t, filesystem, "/d/"+strings.Repeat("x", 300), geometry[0], geometry[1])
		h.keys(terminal.KeyBackspace, terminal.KeyF7)
		h.typeText(strings.Repeat("ж", 200))
		h.keys(terminal.KeyEscape, terminal.KeyF1)
		h.keys(terminal.KeyF1, terminal.KeyEnd, terminal.KeyF8)
		h.keys(terminal.KeyEscape)
		top := h.screen.line(0)
		if !strings.HasPrefix(top, "┌─ /d ") || !strings.Contains(top, "┐┌─ ...") {
			t.Fatalf("%v top row %q", geometry, top)
		}
		rows := min(geometry[1], maxRows)
		if last := h.screen.line(rows - 1); !strings.HasPrefix(last, "F1 Help") || !strings.Contains(last, "F10 Quit") {
			t.Fatalf("%v key bar %q", geometry, last)
		}
		if !strings.Contains(h.screen.text(), "~") {
			t.Fatalf("%v long name not cut", geometry)
		}
	}
}

func TestTooSmallTerminal(t *testing.T) {
	h := newHarness(t, newMemoryFS(), "/", 60, 20)
	if !h.app.TooSmall() || !strings.Contains(h.screen.line(0), "terminal too small (need at least 80x24)") {
		t.Fatalf("too small:\n%s", h.screen.text())
	}
	h.keys(terminal.KeyEnter, terminal.KeyDown)
	if h.app.Quit() {
		t.Fatal("an ordinary key ended it")
	}
	h.keys(terminal.KeyF10)
	if !h.app.Quit() {
		t.Fatal("F10 did not end it")
	}
}

func TestDisplaySizeIsFixed(t *testing.T) {
	h := newHarness(t, newMemoryFS("/d/", "/d/file"), "/d", 106, 33)
	if h.app.SizeWanted() || h.app.FollowsTerminal() {
		t.Fatal("the framebuffer console was asked for its size")
	}
	h.report(86, 43)
	if h.app.columns != 106 || h.app.rows != 33 {
		t.Fatalf("a report changed the display's geometry to %dx%d", h.app.columns, h.app.rows)
	}
	h.keys(terminal.KeyF2)
	if h.app.SizeWanted() {
		t.Fatal("F2 asked a display for its size")
	}
}

func TestSerialOnlyConsoleFollowsTheTerminal(t *testing.T) {
	h := new(harness)
	h.t, h.app, h.fs = t, new(App), newMemoryFS("/d/")
	h.output = terminal.NewOutput(make([]byte, 1024), func(value []byte) { h.frame = append(h.frame, value...) })
	h.app.Init(h.fs, []byte("/d"), 0, 0, false)
	if h.app.columns != 80 || h.app.rows != 24 || !h.app.FollowsTerminal() || !h.app.SizeWanted() {
		t.Fatalf("before a report = %dx%d", h.app.columns, h.app.rows)
	}
	// The terminal on COM1 is filled whatever its size, and followed as it
	// is resized.
	h.report(136, 43)
	if h.app.columns != 136 || h.app.rows != 43 || string(h.app.Active().Path()) != "/d" {
		t.Fatalf("after a report = %dx%d", h.app.columns, h.app.rows)
	}
	if !strings.HasPrefix(h.screen.line(42), "F1 Help") || !strings.HasSuffix(h.screen.line(0), "┐") || len([]rune(h.screen.line(0))) != 136 {
		t.Fatalf("layout at 136x43:\n%s", h.screen.text())
	}
	h.report(86, 30)
	if h.app.columns != 86 || h.app.rows != 30 {
		t.Fatalf("after shrinking = %dx%d", h.app.columns, h.app.rows)
	}
	h.report(70, 20)
	if !h.app.TooSmall() || !strings.Contains(h.screen.line(0), "too small") {
		t.Fatalf("too small:\n%s", h.screen.text())
	}
	h.report(100, 30)
	if h.app.TooSmall() || h.selected() != ".." || h.app.Keys != 0 {
		t.Fatalf("regrown: small %v keys %d", h.app.TooSmall(), h.app.Keys)
	}
	h.keys(terminal.KeyF2)
	if !h.app.SizeWanted() {
		t.Fatal("F2 did not ask the terminal for its size")
	}
	// A framebuffer too small from the start lists the panels once the
	// screen grows.
	small := new(App)
	small.Init(h.fs, []byte("/d"), 66, 25, true)
	if !small.TooSmall() || small.Active().Count() != 0 {
		t.Fatal("a too small screen listed the panels")
	}
	small.displayColumns = 100
	small.resize()
	if small.TooSmall() || small.Active().Count() != 1 {
		t.Fatalf("grown: small %v count %d", small.TooSmall(), small.Active().Count())
	}
}

func TestRenderSendsOnlyChangedRows(t *testing.T) {
	paths := []string{"/d/"}
	for index := 0; index < 30; index++ {
		paths = append(paths, fmt.Sprintf("/d/f%02d", index))
	}
	h := newHarness(t, newMemoryFS(paths...), "/d", 106, 33)
	var sent []byte
	output := terminal.NewOutput(make([]byte, 256), func(value []byte) { sent = append(sent, value...) })
	h.app.Render(&output)
	if len(sent) != 0 {
		t.Fatalf("an unchanged frame sent %d bytes", len(sent))
	}
	h.app.Handle(terminal.Event{Key: terminal.KeyDown})
	h.app.Render(&output)
	// The two list rows, both information lines' panel row and the status line.
	if moves := strings.Count(string(sent), "H"); moves > 4 || len(sent) > 2048 {
		t.Fatalf("moving the selection sent %d rows, %d bytes", moves, len(sent))
	}
	sent = sent[:0]
	h.app.Handle(terminal.Event{Key: terminal.KeyF2})
	h.app.Render(&output)
	if !strings.Contains(string(sent), "\x1b[2J") {
		t.Fatal("reread did not repaint the screen")
	}
}

func TestActivePanelIsShownByColour(t *testing.T) {
	h := newHarness(t, newMemoryFS("/a", "/b"), "/", 80, 24)
	var sent []byte
	output := terminal.NewOutput(make([]byte, 4096), func(value []byte) { sent = append(sent, value...) })
	h.app.screen.invalidate()
	h.app.Render(&output)
	active := fmt.Sprintf("\x1b[%d;%dm", terminal.ForegroundCode(colorSelectionFg), terminal.BackgroundCode(colorSelectionBg))
	inactive := fmt.Sprintf("\x1b[%d;%dm", terminal.ForegroundCode(colorSelectionFg), terminal.BackgroundCode(colorInactiveCursor))
	if strings.Count(string(sent), active) != 1 || strings.Count(string(sent), inactive) != 1 {
		t.Fatalf("selection styles: active %d inactive %d", strings.Count(string(sent), active), strings.Count(string(sent), inactive))
	}
}

func TestSizesAndTruncatedListing(t *testing.T) {
	for size, want := range map[uint64]string{0: "0", 9_999_999: "9999999", 10_000_000: "9.5M",
		706_740_224: "674M", 1_288_490_189: "1.2G", 1 << 62: "4.0E"} {
		if got := string(appendSize(nil, size)); got != want {
			t.Fatalf("size %d = %q, want %q", size, got, want)
		}
	}
	var panel Panel
	panel.SetPath([]byte("/"))
	for index := 0; index < MaxEntries+5; index++ {
		panel.Visit([]byte(fmt.Sprintf("n%04d", index)), ramfs.KindFile, 1, ramfs.Timestamp{})
	}
	if panel.Count() != MaxEntries || !panel.Truncated() {
		t.Fatalf("count %d truncated %v", panel.Count(), panel.Truncated())
	}
	var pool Panel
	long := make([]byte, 1024)
	for index := 0; index < 100; index++ {
		pool.Visit(long, ramfs.KindFile, 1, ramfs.Timestamp{})
	}
	if !pool.Truncated() || pool.Count() != namePoolSize/1024 {
		t.Fatalf("name pool: count %d truncated %v", pool.Count(), pool.Truncated())
	}
}

func TestTruncatedListingIsShown(t *testing.T) {
	paths := []string{"/d/"}
	for index := 0; index < MaxEntries+3; index++ {
		paths = append(paths, fmt.Sprintf("/d/f%04d", index))
	}
	h := newHarness(t, newMemoryFS(paths...), "/d", 80, 24)
	if !strings.Contains(h.screen.text(), "entries, list truncated") {
		t.Fatalf("truncation not shown:\n%s", h.screen.text())
	}
}

func TestRenderAllocatesNothing(t *testing.T) {
	h := newHarness(t, newMemoryFS("/d/", "/d/Файл", "/d/b/"), "/d", 106, 33)
	output := terminal.NewOutput(make([]byte, 1024), func([]byte) {})
	allocations := testing.AllocsPerRun(20, func() {
		h.app.Handle(terminal.Event{Key: terminal.KeyDown})
		h.app.Handle(terminal.Event{Key: terminal.KeyUp})
		h.app.screen.invalidate()
		h.app.Render(&output)
	})
	if allocations != 0 {
		t.Fatalf("a frame allocated %.1f objects", allocations)
	}
}
