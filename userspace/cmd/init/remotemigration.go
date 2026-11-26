//go:build goos_userspace

package main

import (
	configproto "goos/userspace/config"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
	syslog "goos/userspace/log"
	"goos/userspace/remotepolicy"
)

// remoteMigrationKeys is the bounded buffer the legacy authorized_keys file is
// read into. The parser's own limit, not the on-disk size, is the bound.
var remoteMigrationKeys [remotepolicy.MaxAuthorizedKeysBytes]byte

// remoteMigrationDone records that the legacy remote settings have been
// imported into the account database. It is set only on a committed import and
// is never reset, so a missing, corrupt, or ambiguous candidate is retried on a
// later admission rather than silently accepted.
var remoteMigrationDone bool

// remoteMigratedAccount is the name of the account the legacy import created
// or matched. Key mutations applied through the legacy remote command are
// mirrored into this account once the import has committed.
var (
	remoteMigratedAccount    [identity.MaxName]byte
	remoteMigratedAccountLen uint8
)

// migrateRemoteIdentity imports the legacy system.remote.user,
// system.remote.authorized-keys and system.remote.sftp-root settings into one
// identity account. It is idempotent: a re-run adds no duplicate keys (identityd
// dedups by fingerprint), and an already-migrated account is left alone unless
// its home conflicts, which is reported and left for administrative resolution.
// A committed migration makes the account database the sole account/key
// authority; admission never falls back to the legacy authorized_keys file.
//
// When the candidate is missing, corrupt, or ambiguous it fails closed and
// reports a warning, leaving the migration pending. The legacy settings are
// never imported into more than one account, and only validated Ed25519 public
// keys are accepted; passwords and private material cannot exist here.
func migrateRemoteIdentity() bool {
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		return false
	}
	defer closeOpen(settings)
	user, ok := remoteConfigText(settings, "system.remote.user")
	if !ok {
		return false
	}
	home, ok := remoteConfigText(settings, "system.remote.sftp-root")
	if !ok {
		return false
	}
	keysPath, ok := remoteConfigText(settings, "system.remote.authorized-keys")
	if !ok {
		return false
	}
	files, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("remote identity migration: no filesystem session", syslog.Str("reason", reason))
		return false
	}
	size, read := readSessionFile(files, []byte(keysPath), remoteMigrationKeys[:])
	closeOpen(files)
	if !read {
		syslog.Warn("remote identity migration: authorized keys unreadable", syslog.Str("path", keysPath))
		return false
	}
	plan, err := remotepolicy.PlanMigration(user, home, remoteMigrationKeys[:size])
	if err != nil {
		syslog.Warn("remote identity migration rejected", syslog.Str("reason", err.Error()))
		return false
	}
	packed := make([][]byte, len(plan.Keys))
	for index, key := range plan.Keys {
		packed[index] = key
	}
	// One durable identityd commit imports the account, its home, every key
	// and the persisted marker together. No partially applied import can be
	// observed, and a second import is an ErrExists no-op.
	reply, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminImportLegacy,
		Name: []byte(plan.Name), Home: []byte(plan.Home), Key: identityproto.EncodeImportKeys(packed)})
	if !ok {
		syslog.Warn("remote identity migration: identity unavailable")
		return false
	}
	if reply.Result == identityproto.ResultExists {
		// The persisted marker (or the account) already exists: the import is
		// complete, so treat the request as a successful no-op.
		remoteMigratedAccountLen = uint8(copy(remoteMigratedAccount[:], plan.Name))
		return true
	}
	if reply.Result != identityproto.ResultOK {
		syslog.Warn("remote identity migration rejected", syslog.Uint("result", uint64(reply.Result)))
		return false
	}
	remoteMigratedAccountLen = uint8(copy(remoteMigratedAccount[:], plan.Name))
	syslog.Info("remote identity migration committed", syslog.Str("user", plan.Name))
	return true
}
