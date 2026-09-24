package utility

// SizeUnit is the unit a command reports a size in.
type SizeUnit uint8

const (
	UnitBytes SizeUnit = iota
	UnitKiB
	UnitMiB
	UnitGiB
)

// SizeUnitFlag maps -b, -k, -m and -g to their unit.
func SizeUnitFlag(flag []byte) (SizeUnit, bool) {
	if len(flag) != 2 || flag[0] != '-' {
		return 0, false
	}
	switch flag[1] {
	case 'b':
		return UnitBytes, true
	case 'k':
		return UnitKiB, true
	case 'm':
		return UnitMiB, true
	case 'g':
		return UnitGiB, true
	}
	return 0, false
}

// AppendSize appends size in unit: "2519 bytes", or the value with two
// decimals rounded half up followed by the exact byte count, as in
// "2.46 KiB (2519 bytes)". The unit is binary: a KiB is 1024 bytes.
func AppendSize(target []byte, size uint64, unit SizeUnit) []byte {
	var divisor uint64
	var name string
	switch unit {
	case UnitKiB:
		divisor, name = 1<<10, " KiB"
	case UnitMiB:
		divisor, name = 1<<20, " MiB"
	case UnitGiB:
		divisor, name = 1<<30, " GiB"
	default:
		return append(appendUint(target, size), " bytes"...)
	}
	// The remainder is below the divisor, at most 2^30, so scaling it by 100
	// cannot overflow whatever the size.
	whole := size / divisor
	hundredths := (size%divisor*100 + divisor/2) / divisor
	if hundredths == 100 {
		whole, hundredths = whole+1, 0
	}
	target = appendUint(target, whole)
	target = append(target, '.', byte(hundredths/10)+'0', byte(hundredths%10)+'0')
	target = append(target, name...)
	target = append(target, " ("...)
	target = appendUint(target, size)
	return append(target, " bytes)"...)
}

// AppendCount appends "1 file" or "3 files".
func AppendCount(target []byte, count uint64, singular, plural string) []byte {
	target = append(appendUint(target, count), ' ')
	if count == 1 {
		return append(target, singular...)
	}
	return append(target, plural...)
}
