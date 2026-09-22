package logfmt

import (
	"goos/kernel/kcall"
	"testing"
)

func TestNativeCallLimitsMatchRecordFormat(t *testing.T) {
	if kcall.MaxLogRecord != MaxWire || kcall.LogText != MaxText {
		t.Fatalf("kcall limits %d/%d, record format %d/%d", kcall.MaxLogRecord, kcall.LogText, MaxWire, MaxText)
	}
}
