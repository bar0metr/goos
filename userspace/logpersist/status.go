package logpersist

// StatusSuffix names the status file beside the log: the log service
// rewrites logPath+StatusSuffix with its counters, and `log status` prints
// it. The suffix is not a segment number, so rotation never touches it.
const StatusSuffix = ".status"

// MaxStatus bounds the status text AppendStatus writes.
const MaxStatus = 512

// AppendStatus writes the counters and the rotation policy as one
// "name value" line each.
func AppendStatus(target []byte, stats Stats, policy Policy) []byte {
	lines := [...]struct {
		name  string
		value uint64
	}{
		{"appended", stats.Appended}, {"overwritten", stats.Overwritten}, {"persisted", stats.Persisted},
		{"lost", stats.Lost}, {"failures", stats.Failures}, {"rotations", stats.Rotations},
		{"rotation-failures", stats.RotationFailures}, {"repaired", stats.Repaired},
		{"repaired-bytes", stats.RepairedBytes}, {"segment-bytes", policy.SegmentBytes},
		{"segments", uint64(policy.Segments)},
	}
	for _, line := range lines {
		target = append(target, line.name...)
		target = append(target, ' ')
		target = appendDecimal(target, line.value)
		target = append(target, '\n')
	}
	return target
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}
