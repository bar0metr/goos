// Package logpersist moves records from the kernel log ring into rotated
// files. The log service supplies the ring and the filesystem; this package
// owns the cursor, the file format, recovery after a crash, and rotation, so
// host tests can drive all of them against fakes.
package logpersist

// Policy bounds the files: SegmentBytes per segment and Segments files in
// all, the live one included.
type Policy struct {
	SegmentBytes uint64
	Segments     int
}

const (
	// A release build keeps 4 segments of 16 MiB: 64 MiB of history.
	DefaultSegmentBytes = uint64(16 << 20)
	DefaultSegments     = 4
	// A boot produces a few kilobytes of records, so a validation build uses
	// a bound one session crosses and exercises the rotation a release build
	// performs.
	ValidationSegmentBytes = uint64(1536)
	// MinSegmentBytes holds several of the longest lines. MaxSegmentBytes
	// stays far below the FAT32 file limit of 4 GiB - 1.
	MinSegmentBytes = uint64(1024)
	MaxSegmentBytes = uint64(1 << 30)
	// Segment names carry one digit: system.log.1 to system.log.8.
	MinSegments = 2
	MaxSegments = 9
)

// DefaultPolicy is the build's policy before configuration overrides it.
func DefaultPolicy(validation bool) Policy {
	if validation {
		return Policy{SegmentBytes: ValidationSegmentBytes, Segments: DefaultSegments}
	}
	return Policy{SegmentBytes: DefaultSegmentBytes, Segments: DefaultSegments}
}

func (policy Policy) Valid() bool {
	return policy.SegmentBytes >= MinSegmentBytes && policy.SegmentBytes <= MaxSegmentBytes &&
		policy.Segments >= MinSegments && policy.Segments <= MaxSegments
}
