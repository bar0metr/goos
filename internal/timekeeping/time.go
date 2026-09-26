// Package timekeeping defines GoOS clock identifiers and its fixed-width
// timestamp. It contains no firmware or architecture details.
package timekeeping

const NanosPerSecond = uint32(1_000_000_000)

type Clock uint64

const (
	Monotonic Clock = iota + 1
	Realtime
)

// Time is seconds and nanoseconds on a clock's timeline. Realtime seconds are
// signed Unix seconds; monotonic seconds count from boot.
type Time struct {
	Seconds     int64
	Nanoseconds uint32
}

func (value Time) Valid() bool { return value.Nanoseconds < NanosPerSecond }

func (value Time) Before(other Time) bool {
	return value.Seconds < other.Seconds || value.Seconds == other.Seconds && value.Nanoseconds < other.Nanoseconds
}

// Add adds a non-negative duration without reducing the timestamp.
func (value Time) Add(nanos uint64) Time {
	value.Seconds += int64(nanos / uint64(NanosPerSecond))
	fraction := uint64(value.Nanoseconds) + nanos%uint64(NanosPerSecond)
	value.Seconds += int64(fraction / uint64(NanosPerSecond))
	value.Nanoseconds = uint32(fraction % uint64(NanosPerSecond))
	return value
}

func FromNanos(nanos uint64) Time {
	return Time{Seconds: int64(nanos / uint64(NanosPerSecond)), Nanoseconds: uint32(nanos % uint64(NanosPerSecond))}
}

func IsLeapYear(year int64) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

func daysInMonth(year int64, month uint8) uint8 {
	switch month {
	case 2:
		if IsLeapYear(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

// Unix converts a UTC Gregorian calendar value to a Unix timestamp. offset is
// the calendar's minutes east of UTC; the UEFI unspecified value is handled
// by the firmware adapter before this boundary.
func Unix(year int64, month, day, hour, minute, second uint8, nanosecond uint32, offset int16) (Time, bool) {
	if year < 1900 || year > 9999 || month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) ||
		hour > 23 || minute > 59 || second > 59 || nanosecond >= NanosPerSecond || offset < -1440 || offset > 1440 {
		return Time{}, false
	}
	days := daysFromCivil(year, int64(month), int64(day))
	seconds := days*86400 + int64(hour)*3600 + int64(minute)*60 + int64(second) - int64(offset)*60
	return Time{Seconds: seconds, Nanoseconds: nanosecond}, true
}

// CivilUTC converts Unix seconds to a UTC Gregorian calendar value.
func CivilUTC(seconds int64) (year int64, month, day, hour, minute, second uint8) {
	days := floorDiv(seconds, 86400)
	within := seconds - days*86400
	year, month64, day64 := civilFromDays(days)
	return year, uint8(month64), uint8(day64), uint8(within / 3600), uint8(within / 60 % 60), uint8(within % 60)
}

func floorDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor < 0 {
		quotient--
	}
	return quotient
}

func daysFromCivil(year, month, day int64) int64 {
	if month <= 2 {
		year--
	}
	era := floorDiv(year, 400)
	yearOfEra := year - era*400
	shifted := month - 3
	if month <= 2 {
		shifted = month + 9
	}
	dayOfYear := (153*shifted+2)/5 + day - 1
	return era*146097 + yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear - 719468
}

func civilFromDays(days int64) (int64, int64, int64) {
	days += 719468
	era := floorDiv(days, 146097)
	dayOfEra := days - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	year := yearOfEra + era*400
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	shifted := (5*dayOfYear + 2) / 153
	day := dayOfYear - (153*shifted+2)/5 + 1
	month := shifted + 3
	if shifted >= 10 {
		month = shifted - 9
		year++
	}
	return year, month, day
}
