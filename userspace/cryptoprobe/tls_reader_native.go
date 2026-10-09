//go:build cryptoprobe_tls && goos_userspace

package cryptoprobe

import (
	"goos/userspace/cryptosys"
	"io"
)

func tlsProbeReader() io.Reader { return cryptosys.Reader{} }
