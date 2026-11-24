// Package identity implements the M4 identity/account core: a bounded,
// host-testable account/group/key store with generation-safe internal IDs and
// a deterministic single-snapshot codec (ADR 0233).
//
// The package imports only the standard library and goos/internal/limits. It
// never touches goos/kernel, kcall, or arena code, so it runs under plain
// `go test` on the host.
package identity

import "goos/internal/limits"

const (
	MaxAccounts            = 64
	MaxGroups              = 64
	MaxSupplementaryGroups = 16
	MaxName                = 31
	MaxHome                = 64
	MaxKeysPerAccount      = 8
	MaxTotalKeys           = 128
	KeySize                = 32
	FingerprintSize        = 32
	MaxSnapshotBytes       = 65536

	// ReservedUID and ReservedGID are the reserved Unix numbers. Both UIDs and
	// GIDs reject 0 and 0xFFFFFFFF.
	ReservedUID = uint32(0)
	ReservedGID = uint32(0xFFFFFFFF)

	// AllocBase is the first allocatable Unix number; allocation picks the
	// lowest unused value at or above AllocBase.
	AllocBase = uint32(1000)
)

// Identity bounds stay within the shared native limits. These declarations
// fail to compile if a bound ever exceeds its native ceiling.
var (
	_ [limits.NameMax - MaxName + 1]byte
	_ [limits.PathMax - MaxHome + 1]byte
)
