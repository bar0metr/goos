package utility

import "testing"

func TestAppendSize(t *testing.T) {
	cases := []struct {
		size uint64
		unit SizeUnit
		want string
	}{
		{0, UnitBytes, "0 bytes"},
		{2519, UnitBytes, "2519 bytes"},
		{0, UnitKiB, "0.00 KiB (0 bytes)"},
		{2519, UnitKiB, "2.46 KiB (2519 bytes)"},
		{1023, UnitKiB, "1.00 KiB (1023 bytes)"},
		{1024, UnitKiB, "1.00 KiB (1024 bytes)"},
		{5, UnitKiB, "0.00 KiB (5 bytes)"},
		{6, UnitKiB, "0.01 KiB (6 bytes)"},
		{2170880, UnitMiB, "2.07 MiB (2170880 bytes)"},
		{1<<20 - 1, UnitMiB, "1.00 MiB (1048575 bytes)"},
		{3 << 30, UnitGiB, "3.00 GiB (3221225472 bytes)"},
		{^uint64(0), UnitGiB, "17179869184.00 GiB (18446744073709551615 bytes)"},
	}
	for _, c := range cases {
		if got := string(AppendSize(nil, c.size, c.unit)); got != c.want {
			t.Errorf("AppendSize(%d, %d) = %q, want %q", c.size, c.unit, got, c.want)
		}
	}
}

func TestSizeUnitFlag(t *testing.T) {
	for flag, want := range map[string]SizeUnit{"-b": UnitBytes, "-k": UnitKiB, "-m": UnitMiB, "-g": UnitGiB} {
		if unit, ok := SizeUnitFlag([]byte(flag)); !ok || unit != want {
			t.Errorf("SizeUnitFlag(%q) = %d, %v", flag, unit, ok)
		}
	}
	for _, flag := range []string{"", "-", "m", "-x", "-mb", "--m", "/tmp"} {
		if _, ok := SizeUnitFlag([]byte(flag)); ok {
			t.Errorf("SizeUnitFlag(%q) accepted", flag)
		}
	}
}

func TestAppendCount(t *testing.T) {
	if got := string(AppendCount(nil, 1, "file", "files")); got != "1 file" {
		t.Errorf("got %q", got)
	}
	if got := string(AppendCount(nil, 0, "file", "files")); got != "0 files" {
		t.Errorf("got %q", got)
	}
}
