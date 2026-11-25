// Package remotepolicy validates remote access settings and persistent keys.
package remotepolicy

import (
	"crypto/ed25519"
	"errors"

	"goos/userspace/identity"
)

// MigrationPlan is the validated single-account import derived from the legacy
// system.remote.user, system.remote.authorized-keys and system.remote.sftp-root
// settings. It carries only validated Ed25519 public keys, never passwords or
// private material, and never widens the account's authority.
type MigrationPlan struct {
	Name string
	Home string
	Keys []ed25519.PublicKey
}

// PlanMigration validates the legacy remote settings into one account import.
// The user name must match the identity name grammar, the SFTP root must be a
// canonical path strictly below /home, and the authorized keys are parsed in
// whole: any unsupported key, option, malformed blob or bound violation rejects
// the entire candidate. It reads no filesystem; the caller supplies the already
// read key bytes, so the decision is deterministic and host-testable.
func PlanMigration(user, sftpRoot string, authorizedKeys []byte) (MigrationPlan, error) {
	if !identity.ValidName(user) {
		return MigrationPlan{}, errors.New("invalid legacy remote user")
	}
	if !ValidSFTPRoot(sftpRoot) {
		return MigrationPlan{}, errors.New("invalid legacy remote SFTP root")
	}
	parsed, err := ParseAuthorizedKeys(authorizedKeys)
	if err != nil {
		return MigrationPlan{}, err
	}
	keys := make([]ed25519.PublicKey, parsed.Len())
	for index := range keys {
		keys[index], _ = parsed.At(index)
	}
	return MigrationPlan{Name: user, Home: sftpRoot, Keys: keys}, nil
}
