//go:build uefi && amd64

package uefi

import (
	deviceuefi "device/uefi"
	"goos/internal/timekeeping"
)

const unspecifiedTimezone = int16(0x07ff)

// CalendarTime reads and validates the firmware calendar once. An unspecified
// firmware zone is interpreted as UTC, which is GoOS's system timezone until
// timezone configuration exists.
func CalendarTime() (timekeeping.Time, deviceuefi.EFI_STATUS) {
	value, status := deviceuefi.GetTime()
	if status != deviceuefi.EFI_SUCCESS {
		return timekeeping.Time{}, status
	}
	offset := value.TimeZone
	if offset == unspecifiedTimezone {
		offset = 0
	}
	if value.Daylight&^byte(3) != 0 {
		return timekeeping.Time{}, deviceuefi.EFI_COMPROMISED_DATA
	}
	result, valid := timekeeping.Unix(int64(value.Year), value.Month, value.Day, value.Hour, value.Minute, value.Second, value.Nanosecond, offset)
	if !valid {
		return timekeeping.Time{}, deviceuefi.EFI_COMPROMISED_DATA
	}
	return result, deviceuefi.EFI_SUCCESS
}
