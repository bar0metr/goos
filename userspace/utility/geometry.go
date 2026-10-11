package utility

import (
	"goos/userspace/startup"
	"goos/userspace/tty"
)

// TerminalGeometry separates a fixed display from authoritative peer geometry.
// Legacy COLUMNS/LINES without flags still describe the local framebuffer.
func TerminalGeometry(data startup.Data) (tty.Geometry, bool) {
	columnsText, columnsFound := data.Env("COLUMNS")
	rowsText, rowsFound := data.Env("LINES")
	if !columnsFound || !rowsFound {
		return tty.Geometry{}, false
	}
	columns, columnsOK := geometryUint(columnsText)
	rows, rowsOK := geometryUint(rowsText)
	if !columnsOK || !rowsOK || columns == 0 || rows == 0 || columns > 0xffff || rows > 0xffff {
		return tty.Geometry{}, false
	}
	geometry := tty.Geometry{Columns: int(columns), Rows: int(rows), Display: true, Authoritative: true}
	if flag, found := data.Env(tty.DisplayVariable); found {
		if len(flag) != 1 || flag[0] != '0' && flag[0] != '1' {
			return tty.Geometry{}, false
		}
		geometry.Display = flag[0] == '1'
	}
	if flag, found := data.Env(tty.AuthoritativeVariable); found {
		if len(flag) != 1 || flag[0] != '0' && flag[0] != '1' {
			return tty.Geometry{}, false
		}
		geometry.Authoritative = flag[0] == '1'
	}
	if sequence, found := data.Env(tty.SequenceVariable); found {
		var ok bool
		geometry.Sequence, ok = geometryUint(sequence)
		if !ok {
			return tty.Geometry{}, false
		}
	}
	geometry.Term, _ = data.Env("TERM")
	return geometry, geometry.Valid() && (geometry.Display || geometry.ValidRemote())
}

func TerminalSize(data startup.Data) (int, int, bool) {
	geometry, valid := TerminalGeometry(data)
	return geometry.Columns, geometry.Rows, valid && geometry.Display
}

func geometryUint(text []byte) (uint64, bool) {
	if len(text) == 0 {
		return 0, false
	}
	var value uint64
	for _, digit := range text {
		if digit < '0' || digit > '9' || value > (^uint64(0)-uint64(digit-'0'))/10 {
			return 0, false
		}
		value = value*10 + uint64(digit-'0')
	}
	return value, true
}
