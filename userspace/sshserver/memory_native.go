//go:build goos_sshcompat && goos_userspace

package sshserver

import (
	"goos/userspace/cryptosys"
	"goos/userspace/runtime/arena"
	"io"
)

type pumpMemory struct {
	keys       [2]*arena.Region
	transient  *arena.Region
	generation uint8
	keyScope   arena.Scope
}
type packetScope struct{ scope arena.Scope }

func newPumpMemory() pumpMemory {
	return pumpMemory{keys: [2]*arena.Region{arena.NewRegion(256 << 10), arena.NewRegion(256 << 10)}, transient: arena.NewRegion(256 << 10)}
}
func (m *pumpMemory) EnterKey(newGeneration bool) {
	if newGeneration {
		m.generation ^= 1
		m.keys[m.generation].Clear()
	}
	m.keyScope = m.keys[m.generation].Enter()
}
func (m *pumpMemory) LeaveKey() { m.keyScope.Leave() }
func (m *pumpMemory) packet() packetScope {
	m.transient.Clear()
	return packetScope{scope: m.transient.Enter()}
}
func (s packetScope) leave()           { s.scope.Leave() }
func nativeRandom(io.Reader) io.Reader { return cryptosys.Reader{} }
