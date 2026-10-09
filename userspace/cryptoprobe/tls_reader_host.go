//go:build cryptoprobe_tls && !goos_userspace

package cryptoprobe

import (
	"bytes"
	"io"
)

func tlsProbeReader() io.Reader {
	// Host-only fixture entropy; native TLS always uses kernel entropy.
	var fixture [8192]byte
	for i := range fixture {
		fixture[i] = 0x42
	}
	return bytes.NewReader(fixture[:])
}
