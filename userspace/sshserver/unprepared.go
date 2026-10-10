//go:build !goos_sshcompat

package sshserver

import "net"

func newBackend(net.Conn, Config) (backend, error) { return nil, ErrCompatibilityBuild }
