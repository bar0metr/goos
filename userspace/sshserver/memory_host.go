//go:build goos_sshcompat && !goos_userspace

package sshserver

import "io"

type pumpMemory struct{}
type packetScope struct{}

func newPumpMemory() pumpMemory               { return pumpMemory{} }
func (*pumpMemory) EnterKey(bool)             {}
func (*pumpMemory) LeaveKey()                 {}
func (*pumpMemory) packet() packetScope       { return packetScope{} }
func (packetScope) leave()                    {}
func nativeRandom(reader io.Reader) io.Reader { return reader }
