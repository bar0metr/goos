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
//   - The probe values apply the next time an address is probed, and the
//     announcement values to the next announcement sequence; a sequence
//     already running keeps the values it started with, and a live address is
//     not probed again because they changed.
//   - DefendInterval, MaxConflicts and ConflictRateLimit apply to the next
//     conflict.
//   - DHCP retry values apply at the client's next scheduling point. A lease,
//     its T1, T2 and expiry stay as the server set them.
type Tuning struct {
	TTL               uint8
	ProbeCount        uint8
	ProbeWait         uint64
	ProbeMin          uint64
	ProbeMax          uint64
	AnnounceCount     uint8
	AnnounceInterval  uint64
	DefendInterval    uint64
	MaxConflicts      uint8
	ConflictRateLimit uint64
	NeighborLifetime  uint64
	ResolveInterval   uint64
	ResolveAttempts   uint8
	EchoReplyRate     uint16
	DHCPInitialRetry  uint64
	DHCPMaxRetry      uint64
	DHCPConflictRetry uint64
}

// TuningFields is the number of stored tuning values ParseTuning takes.
const TuningFields = int(lastField-FieldTTL) + 1

// announceWait is RFC 5227's ANNOUNCE_WAIT: the quiet period after the last
// probe before the address goes live. It is fixed, not tuned.
const announceWait = 2 * second

// DefaultTuning is the policy the stack runs with until a configuration says
// otherwise. The address-conflict values are RFC 5227's.
func DefaultTuning() Tuning {
	return Tuning{
		TTL:               64,
		ProbeCount:        3,
		ProbeWait:         second,
		ProbeMin:          second,
		ProbeMax:          2 * second,
		AnnounceCount:     2,
		AnnounceInterval:  2 * second,
		DefendInterval:    10 * second,
		MaxConflicts:      10,
		ConflictRateLimit: 60 * second,
		NeighborLifetime:  60 * second,
		ResolveInterval:   second,
		ResolveAttempts:   3,
		EchoReplyRate:     20,
		DHCPInitialRetry:  4 * second,
		DHCPMaxRetry:      64 * second,
		DHCPConflictRetry: 10 * second,
	}
}

// tuningRange is the accepted range of each stored value, in Field order
// from FieldTTL, and whether it is stored in milliseconds. The largest bound,
// an hour, keeps every deadline far from overflow.
var tuningRange = [TuningFields]struct {
	min, max uint64
	millis   bool
}{
	{1, 255, false},         // FieldTTL
	{1, 10, false},          // FieldProbeCount
	{0, 10_000, true},       // FieldProbeWait
	{100, 10_000, true},     // FieldProbeMin
	{100, 10_000, true},     // FieldProbeMax
	{1, 10, false},          // FieldAnnounceCount
	{100, 10_000, true},     // FieldAnnounceInterval
	{1000, 60_000, true},    // FieldDefendInterval
	{1, 100, false},         // FieldMaxConflicts
	{1000, 600_000, true},   // FieldConflictRateLimit
	{1000, 3_600_000, true}, // FieldARPCacheTimeout
	{100, 10_000, true},     // FieldARPResolveInterval
	{1, 10, false},          // FieldARPResolveAttempts
	{0, 10_000, false},      // FieldEchoReplyRate
	{1000, 60_000, true},    // FieldDHCPInitialRetry
	{4000, 300_000, true},   // FieldDHCPMaxRetry
	{1000, 300_000, true},   // FieldDHCPConflictRetry
}

// ParseTuning builds a tuning from the stored values, given in Field order
// from FieldTTL, and returns the first field out of range. Range checks run
// before any conversion, so no value can wrap into an accepted one.
func ParseTuning(values *[TuningFields]uint64) (Tuning, Field) {
	var converted [TuningFields]uint64
	for index, value := range values {
		limits := tuningRange[index]
		if value < limits.min || value > limits.max {
			return Tuning{}, FieldTTL + Field(index)
		}
		converted[index] = value
		if limits.millis {
			converted[index] = value * millisecond
		}
	}
	tuning := tuningFrom(&converted)
	return tuning, tuning.Validate()
}

func tuningFrom(values *[TuningFields]uint64) Tuning {
	return Tuning{
		TTL:               uint8(values[FieldTTL-FieldTTL]),
		ProbeCount:        uint8(values[FieldProbeCount-FieldTTL]),
		ProbeWait:         values[FieldProbeWait-FieldTTL],
		ProbeMin:          values[FieldProbeMin-FieldTTL],
		ProbeMax:          values[FieldProbeMax-FieldTTL],
		AnnounceCount:     uint8(values[FieldAnnounceCount-FieldTTL]),
		AnnounceInterval:  values[FieldAnnounceInterval-FieldTTL],
		DefendInterval:    values[FieldDefendInterval-FieldTTL],
		MaxConflicts:      uint8(values[FieldMaxConflicts-FieldTTL]),
		ConflictRateLimit: values[FieldConflictRateLimit-FieldTTL],
		NeighborLifetime:  values[FieldARPCacheTimeout-FieldTTL],
		ResolveInterval:   values[FieldARPResolveInterval-FieldTTL],
		ResolveAttempts:   uint8(values[FieldARPResolveAttempts-FieldTTL]),
		EchoReplyRate:     uint16(values[FieldEchoReplyRate-FieldTTL]),
		DHCPInitialRetry:  values[FieldDHCPInitialRetry-FieldTTL],
		DHCPMaxRetry:      values[FieldDHCPMaxRetry-FieldTTL],
		DHCPConflictRetry: values[FieldDHCPConflictRetry-FieldTTL],
	}
}

func (tuning *Tuning) values() [TuningFields]uint64 {
	return [TuningFields]uint64{
		uint64(tuning.TTL),
		uint64(tuning.ProbeCount),
		tuning.ProbeWait,
		tuning.ProbeMin,
		tuning.ProbeMax,
		uint64(tuning.AnnounceCount),
		tuning.AnnounceInterval,
		tuning.DefendInterval,
		uint64(tuning.MaxConflicts),
		tuning.ConflictRateLimit,
		tuning.NeighborLifetime,
		tuning.ResolveInterval,
		uint64(tuning.ResolveAttempts),
		uint64(tuning.EchoReplyRate),
		tuning.DHCPInitialRetry,
		tuning.DHCPMaxRetry,
		tuning.DHCPConflictRetry,
	}
}

// Validate returns the first field outside its range, or the second field of
// a pair out of order: ProbeMax below ProbeMin, DHCPMaxRetry below
// DHCPInitialRetry.
func (tuning Tuning) Validate() Field {
	values := tuning.values()
	for index, value := range values {
		limits := tuningRange[index]
		if limits.millis {
			limits.min, limits.max = limits.min*millisecond, limits.max*millisecond
		}
		if value < limits.min || value > limits.max {
			return FieldTTL + Field(index)
		}
	}
	if tuning.ProbeMax < tuning.ProbeMin {
		return FieldProbeMax
	}
	if tuning.DHCPMaxRetry < tuning.DHCPInitialRetry {
		return FieldDHCPMaxRetry
	}
	return FieldNone
}
