package device

import "testing"

func TestLogObjectHasNoConsoleAuthority(t *testing.T) {
	if !CanWriteLog(LogObject) || IsConsole(LogObject) || IsTerminalConsole(LogObject) {
		t.Fatal("log producer acquired console authority")
	}
	for _, console := range []uint64{ConsoleObject, SerialConsoleObject, GraphicalConsoleObject} {
		if !CanWriteLog(console) {
			t.Fatal("existing console logger rejected", console)
		}
	}
}

func TestUnrelatedDeviceCannotWriteLog(t *testing.T) {
	port, _ := PortObject(0x1000, 16)
	dma, _ := DMAObject(DMADomainNet)
	image, _ := ImageObject(1)
	for _, object := range []uint64{0, SystemControlObject, LoaderObject, ClockObject, InertObject, port, dma, image} {
		if CanWriteLog(object) {
			t.Fatal("unrelated device accepted as log producer", object)
		}
	}
}
