package ext4

import "testing"

func TestSetAttributes(t *testing.T) {
	fs, device := scratch(t, "canonical")
	file := ok(fs.Create(root(t, fs), []byte("mode"), ModeRegular|0o644, 0, 0))
	stamp := Timestamp{Seconds: 1700000000, Nanos: 5}
	if err := fs.SetAttributes(file, AttributeChange{SetMode: true, Mode: ModeDir | 0o400, SetModify: true, Modify: stamp}); err != nil {
		t.Fatal(err)
	}
	changed := lookupInode(t, fs, root(t, fs), "mode")
	if changed.Mode != ModeRegular|0o400 || changed.Modify != stamp {
		t.Fatalf("mode %o modify %+v", changed.Mode, changed.Modify)
	}
	if changed.Change == file.Change {
		t.Fatal("change time kept")
	}
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}
	reread := mount(t, ok(settled(device)).(*scratchDevice))
	again := lookupInode(t, reread, root(t, reread), "mode")
	if again.Mode != ModeRegular|0o400 || again.Modify != stamp {
		t.Fatalf("mode after remount %o", again.Mode)
	}
}
