package network

// Tuning is the timer and rate policy of the stack. Times are in nanoseconds.
// It sizes nothing: tables, sessions and the transmit ring stay fixed arrays,
// so no setting can grow the memory a stack holds.
//
// A new tuning applies at each value's next point of use, never retroactively:
//   - TTL and EchoReplyRate apply to the next packet built or reply counted;
//     frames already queued keep the TTL they were built with.
//   - NeighborLifetime applies to entries learned or refreshed from then on;
//     an existing entry keeps its expiry until its next refresh.
//   - ResolveInterval and ResolveAttempts apply to resolutions started later;
//     one already running finishes under the values it started with.
//   - ProbeCount and ProbeInterval apply the next time an address is probed;
//     a live address is not probed again because they changed.
//   - DHCP retry values apply at the client's next scheduling point. A lease,
//     its T1, T2 and expiry stay as the server set them.
type Tuning struct {
	TTL               uint8
	NeighborLifetime  uint64
	ResolveInterval   uint64
	ResolveAttempts   uint8
	EchoReplyRate     uint16
	DHCPInitialRetry  uint64
	DHCPMaxRetry      uint64
	DHCPConflictRetry uint64
	ProbeCount        uint8
	ProbeInterval     uint64
}

// TuningFields is the number of stored tuning values ParseTuning takes.
const TuningFields = int(lastField-FieldTTL) + 1

// DefaultTuning is the policy the stack runs with until a configuration says
// otherwise.
func DefaultTuning() Tuning {
	return Tuning{
		TTL:               64,
		NeighborLifetime:  60 * second,
		ResolveInterval:   second,
		ResolveAttempts:   3,
		EchoReplyRate:     20,
		DHCPInitialRetry:  4 * second,
		DHCPMaxRetry:      64 * second,
		DHCPConflictRetry: 10 * second,
		ProbeCount:        2,
		ProbeInterval:     500 * millisecond,
	}
}

// tuningRange is the accepted range of each stored value, in Field order
// from FieldTTL. Times are in milliseconds, as they are stored. The largest
// bound, an hour, keeps every deadline far from overflow.
var tuningRange = [TuningFields]struct{ min, max uint64 }{
	{1, 255},          // FieldTTL
	{1000, 3_600_000}, // FieldARPCacheTimeout
	{100, 10_000},     // FieldARPResolveInterval
	{1, 10},           // FieldARPResolveAttempts
	{0, 10_000},       // FieldEchoReplyRate
	{1000, 60_000},    // FieldDHCPInitialRetry
	{4000, 300_000},   // FieldDHCPMaxRetry
	{1000, 300_000},   // FieldDHCPConflictRetry
	{1, 10},           // FieldProbeCount
	{100, 5000},       // FieldProbeInterval
}

// ParseTuning builds a tuning from the stored values, given in Field order
// from FieldTTL, and returns the first field out of range. Range checks run
// before any conversion, so no value can wrap into an accepted one.
func ParseTuning(values *[TuningFields]uint64) (Tuning, Field) {
	for index, value := range values {
		if value < tuningRange[index].min || value > tuningRange[index].max {
			return Tuning{}, FieldTTL + Field(index)
		}
	}
	tuning := Tuning{
		TTL:               uint8(values[FieldTTL-FieldTTL]),
		NeighborLifetime:  values[FieldARPCacheTimeout-FieldTTL] * millisecond,
		ResolveInterval:   values[FieldARPResolveInterval-FieldTTL] * millisecond,
		ResolveAttempts:   uint8(values[FieldARPResolveAttempts-FieldTTL]),
		EchoReplyRate:     uint16(values[FieldEchoReplyRate-FieldTTL]),
		DHCPInitialRetry:  values[FieldDHCPInitialRetry-FieldTTL] * millisecond,
		DHCPMaxRetry:      values[FieldDHCPMaxRetry-FieldTTL] * millisecond,
		DHCPConflictRetry: values[FieldDHCPConflictRetry-FieldTTL] * millisecond,
		ProbeCount:        uint8(values[FieldProbeCount-FieldTTL]),
		ProbeInterval:     values[FieldProbeInterval-FieldTTL] * millisecond,
	}
	return tuning, tuning.Validate()
}

// Validate returns the first field outside its range, or FieldDHCPMaxRetry
// when the backoff ceiling is below its starting value.
func (tuning Tuning) Validate() Field {
	values := [TuningFields]uint64{
		uint64(tuning.TTL),
		tuning.NeighborLifetime,
		tuning.ResolveInterval,
		uint64(tuning.ResolveAttempts),
		uint64(tuning.EchoReplyRate),
		tuning.DHCPInitialRetry,
		tuning.DHCPMaxRetry,
		tuning.DHCPConflictRetry,
		uint64(tuning.ProbeCount),
		tuning.ProbeInterval,
	}
	for index, value := range values {
		limits := tuningRange[index]
		if tuningMillis[index] {
			limits.min, limits.max = limits.min*millisecond, limits.max*millisecond
		}
		if value < limits.min || value > limits.max {
			return FieldTTL + Field(index)
		}
	}
	if tuning.DHCPMaxRetry < tuning.DHCPInitialRetry {
		return FieldDHCPMaxRetry
	}
	return FieldNone
}

// tuningMillis marks the values stored in milliseconds.
var tuningMillis = [TuningFields]bool{false, true, true, false, false, true, true, true, false, true}
