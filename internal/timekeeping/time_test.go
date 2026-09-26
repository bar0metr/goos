package timekeeping

import "testing"

func TestUnixEpochAndLeapYears(t *testing.T) {
	for _, test := range []struct {
		year        int64
		month, day  uint8
		wantSeconds int64
	}{
		{1970, 1, 1, 0},
		{2000, 2, 29, 951782400},
		{2100, 3, 1, 4107542400},
	} {
		got, ok := Unix(test.year, test.month, test.day, 0, 0, 0, 0, 0)
		if !ok || got.Seconds != test.wantSeconds {
			t.Fatalf("%d-%02d-%02d = %+v, %v", test.year, test.month, test.day, got, ok)
		}
		year, month, day, _, _, _ := CivilUTC(got.Seconds)
		if year != test.year || month != test.month || day != test.day {
			t.Fatalf("%d became %d-%02d-%02d", got.Seconds, year, month, day)
		}
	}
	if _, ok := Unix(2100, 2, 29, 0, 0, 0, 0, 0); ok {
		t.Fatal("2100-02-29 accepted")
	}
}

func TestUnixConvertsOffsetToUTC(t *testing.T) {
	value, ok := Unix(1970, 1, 1, 3, 0, 0, 123, 180)
	if !ok || value != (Time{Nanoseconds: 123}) {
		t.Fatalf("offset conversion = %+v, %v", value, ok)
	}
}

func TestInvalidCalendarIsRejected(t *testing.T) {
	for _, test := range []struct {
		year                             int64
		month, day, hour, minute, second uint8
		nanos                            uint32
		offset                           int16
	}{
		{2024, 0, 1, 0, 0, 0, 0, 0},
		{2023, 2, 29, 0, 0, 0, 0, 0},
		{2024, 1, 1, 24, 0, 0, 0, 0},
		{2024, 1, 1, 0, 0, 60, 0, 0},
		{2024, 1, 1, 0, 0, 0, NanosPerSecond, 0},
		{2024, 1, 1, 0, 0, 0, 0, 1441},
	} {
		if _, ok := Unix(test.year, test.month, test.day, test.hour, test.minute, test.second, test.nanos, test.offset); ok {
			t.Fatalf("invalid calendar accepted: %+v", test)
		}
	}
}

func TestAddNormalizesNanoseconds(t *testing.T) {
	got := (Time{Seconds: 10, Nanoseconds: 900_000_000}).Add(2_300_000_000)
	if got != (Time{Seconds: 13, Nanoseconds: 200_000_000}) {
		t.Fatalf("add = %+v", got)
	}
}
