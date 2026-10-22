package main

import (
	"goos/internal/logfmt"
	"goos/kernel/vfs"
	"goos/userspace/utility"
)

// appendInstalledFailure is one console record, outside the structured log's
// 96-byte field budget. Paths here come from the protected installed index.
func appendInstalledFailure(out []byte, path, phase string, session, correlation, elapsed uint64, received bool, status vfs.Status, failure utility.VFSFailure) []byte {
	out = append(out, "INSTALLED_READ_FAIL path="...)
	out = append(out, path...)
	out = append(out, " phase="...)
	out = append(out, phase...)
	out = append(out, " session="...)
	out = logfmt.AppendUint(out, session)
	out = append(out, " correlation="...)
	out = logfmt.AppendUint(out, correlation)
	out = append(out, " elapsed="...)
	out = logfmt.AppendUint(out, elapsed)
	out = append(out, " received="...)
	if received {
		out = append(out, '1')
	} else {
		out = append(out, '0')
	}
	out = append(out, " vfs="...)
	out = logfmt.AppendUint(out, uint64(status))
	out = append(out, " transaction="...)
	out = append(out, failure.Phase...)
	out = append(out, " raw="...)
	out = logfmt.AppendUint(out, uint64(failure.Status))
	out = append(out, " sent="...)
	if failure.Sent {
		out = append(out, '1')
	} else {
		out = append(out, '0')
	}
	out = append(out, " reply-correlation="...)
	out = logfmt.AppendUint(out, failure.ReplyCorrelation)
	out = append(out, " reply-operation="...)
	out = logfmt.AppendUint(out, uint64(failure.ReplyOperation))
	return append(out, '\n')
}
