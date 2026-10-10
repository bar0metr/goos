//go:build !cryptoprobe_ed25519 && !cryptoprobe_sha256 && !cryptoprobe_sha512 && !cryptoprobe_hmac && !cryptoprobe_aes && !cryptoprobe_cipher && !cryptoprobe_ecdh && !cryptoprobe_ecdsa && !cryptoprobe_rsa && !cryptoprobe_rand && !cryptoprobe_x509 && !cryptoprobe_asn1 && !cryptoprobe_pem && !cryptoprobe_tls

// Package cryptoprobe contains isolated native crypto feasibility checks.
package cryptoprobe

import (
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/userspace/cryptosys"
	"goos/userspace/runtime/sys"
)

const Name = "platform"

type platformError string

func (e platformError) Error() string { return string(e) }

func Run() error {
	var buffer [131]byte
	for _, size := range []int{0, 1, 8, 63, 64, 65, 129} {
		buffer[0], buffer[size+1] = 0xa5, 0x5a
		n, err := (cryptosys.Reader{}).Read(buffer[1 : size+1])
		if n != size || err != nil {
			return platformError("kernel random read failed")
		}
		if buffer[0] != 0xa5 || buffer[size+1] != 0x5a {
			return platformError("kernel random read exceeded buffer")
		}
	}
	first, result := sys.ClockNow(timekeeping.Realtime)
	if result != kcall.StatusOK || first.Seconds <= 0 || first.Nanoseconds >= 1000000000 {
		return platformError("realtime unavailable or invalid")
	}
	second, result := sys.ClockNow(timekeeping.Realtime)
	if result != kcall.StatusOK || second.Seconds < first.Seconds || second.Seconds == first.Seconds && second.Nanoseconds < first.Nanoseconds {
		return platformError("realtime rolled back")
	}
	if _, result := sys.ClockNow(timekeeping.Monotonic); result != kcall.StatusOK {
		return platformError("monotonic clock unavailable")
	}
	return nil
}
