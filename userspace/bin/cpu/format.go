package main

import "goos/kernel/kcall"

// appendTable formats one line per processor. The compact table gives the
// busy and idle shares of each processor's ticks; raw gives the counters.
// TASK is the scheduler slot of the task the processor ran when it was read.
func appendTable(target []byte, records []kcall.CPURuntime, raw bool) []byte {
	if raw {
		target = appendRow(target, "CPU", "STATE", "TICKS", "IDLE", "TASK", "SWITCHES", "STEALS", "MIGRATIONS", "RESCHED", "SHOOTDOWN")
	} else {
		target = appendRow(target, "CPU", "STATE", "BUSY", "IDLE", "TASK", "SWITCHES", "STEALS", "MIGRATIONS")
	}
	var cells [10][24]byte
	for _, record := range records {
		state := "offline"
		if record.Online {
			state = "online"
		}
		columns := [10][]byte{
			appendUint(cells[0][:0], uint64(record.Index)),
			append(cells[1][:0], state...),
		}
		if raw {
			columns[2] = appendUint(cells[2][:0], record.Ticks)
			columns[3] = appendUint(cells[3][:0], record.IdleTicks)
		} else {
			busy := record.Busy()
			columns[2] = append(appendUint(cells[2][:0], busy), '%')
			columns[3] = append(appendUint(cells[3][:0], 100-busy), '%')
		}
		columns[4] = appendTask(cells[4][:0], record)
		columns[5] = appendUint(cells[5][:0], record.Switches)
		columns[6] = appendUint(cells[6][:0], record.Steals)
		columns[7] = appendUint(cells[7][:0], record.Migrations)
		count := 8
		if raw {
			columns[8] = appendUint(cells[8][:0], record.Reschedules)
			columns[9] = appendUint(cells[9][:0], record.Shootdowns)
			count = 10
		}
		for index := 0; index < count; index++ {
			target = appendCell(target, columns[index], index, index == count-1)
		}
	}
	return target
}

// widths are the column widths; the last column is not padded.
var widths = [10]int{4, 8, 7, 7, 6, 10, 8, 12, 9, 9}

func appendRow(target []byte, names ...string) []byte {
	for index, name := range names {
		target = appendCell(target, []byte(name), index, index == len(names)-1)
	}
	return target
}

func appendCell(target []byte, cell []byte, column int, last bool) []byte {
	target = append(target, cell...)
	if last {
		return append(target, '\n')
	}
	for pad := len(cell); pad < widths[column]; pad++ {
		target = append(target, ' ')
	}
	if len(cell) >= widths[column] {
		target = append(target, ' ')
	}
	return target
}

// appendTask writes the scheduler slot of the processor's task, or idle. An
// offline processor runs nothing.
func appendTask(target []byte, record kcall.CPURuntime) []byte {
	slot := uint32(record.Current)
	if !record.Online || record.Current == 0 || slot == 0 {
		if !record.Online {
			return append(target, '-')
		}
		return append(target, "idle"...)
	}
	return appendUint(target, uint64(slot-1))
}

func appendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(target, digits[index:]...)
}
