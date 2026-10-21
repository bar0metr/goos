//go:build !goos_vfs_probe || !goos_goroutines

package vfsservice

import "goos/kernel/vfs"

// ValidationReadKind is a no-op outside the explicit native acceptance build.
type ValidationReadKind uint8

const (
	ValidationReadLocal ValidationReadKind = iota + 1
	ValidationReadPersistent
)

type validationReadGate struct{}

func (service *Service) ArmValidationRead(vfs.Owner, vfs.Handle, ValidationReadKind) bool {
	return false
}
func (service *Service) validationReadGate(vfs.Owner, vfs.Handle, ReadLane) validationReadGate {
	return validationReadGate{}
}
func (validationReadGate) hold()                            {}
func (service *Service) ReleaseValidationPersistentRead()   {}
func (service *Service) ValidationReadLocalOverlap() bool   { return false }
func (service *Service) ValidationPersistentActive() bool   { return false }
func (service *Service) ValidationPersistentTimedOut() bool { return false }
func (service *Service) ValidationPersistentDone() bool     { return false }
