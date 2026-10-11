package admincmd

import "goos/internal/timekeeping"

// Hexdump prints every byte, without duplicate-row suppression.
func Hexdump(reader Reader, writer Writer) Status {
	var input [ChunkSize]byte
	var row [16]byte
	var rendered [80]byte
	var total, offset uint64
	size := 0
	for {
		n, status := read(reader, input[:], &total)
		if status != OK && status != EOF {
			return status
		}
		for _, c := range input[:n] {
			row[size] = c
			size++
			if size == len(row) {
				if result := writer.Write(HexRow(offset, row[:], &rendered)); result != OK {
					return result
				}
				offset += uint64(size)
				size = 0
			}
		}
		if status == EOF {
			if size != 0 {
				return writer.Write(HexRow(offset, row[:size], &rendered))
			}
			return OK
		}
	}
}

func HexRow(offset uint64, data []byte, output *[80]byte) []byte {
	if len(data) > 16 {
		data = data[:16]
	}
	const hex = "0123456789abcdef"
	size := 0
	for shift := 28; shift >= 0; shift -= 4 {
		output[size] = hex[(offset>>uint(shift))&15]
		size++
	}
	output[size], output[size+1] = ' ', ' '
	size += 2
	for index := 0; index < 16; index++ {
		if index < len(data) {
			output[size], output[size+1] = hex[data[index]>>4], hex[data[index]&15]
		} else {
			output[size], output[size+1] = ' ', ' '
		}
		output[size+2] = ' '
		size += 3
	}
	output[size], output[size+1] = ' ', '|'
	size += 2
	for _, c := range data {
		if c < 32 || c > 126 {
			c = '.'
		}
		output[size] = c
		size++
	}
	output[size], output[size+1] = '|', '\n'
	size += 2
	return output[:size]
}

// UTC uses a fixed UTC calendar format, with second precision.
func UTC(value timekeeping.Time, output *[21]byte) ([]byte, bool) {
	// CivilUTC is defined on signed seconds; bound before conversion to avoid
	// overflow at the extremes and keep the four-digit year unambiguous.
	if !value.Valid() || value.Seconds < -62167219200 || value.Seconds > 253402300799 {
		return nil, false
	}
	year, month, day, hour, minute, second := timekeeping.CivilUTC(value.Seconds)
	if year < 0 || year > 9999 {
		return nil, false
	}
	output[0], output[1], output[2], output[3] = byte('0'+year/1000), byte('0'+year/100%10), byte('0'+year/10%10), byte('0'+year%10)
	output[4], output[7], output[10], output[13], output[16], output[19], output[20] = '-', '-', 'T', ':', ':', 'Z', '\n'
	for index, number := range [...]uint8{month, day, hour, minute, second} {
		position := [...]int{5, 8, 11, 14, 17}[index]
		output[position], output[position+1] = '0'+number/10, '0'+number%10
	}
	return output[:], true
}
