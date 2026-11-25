//go:build goos_userspace

package main

import (
	configproto "goos/userspace/config"
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
	account, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminGetAccount, Name: []byte(plan.Name)})
	if !ok {
		syslog.Warn("remote identity migration: identity unavailable")
		return false
	}
	switch account.Result {
	case identityproto.ResultNotFound, identityproto.ResultUnprovisioned:
		if account.Result == identityproto.ResultUnprovisioned {
			// An unprovisioned database is empty; provision it once before the
			// import, which is the explicit defined recovery path.
			if reply, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminProvision}); !ok || reply.Result != identityproto.ResultOK {
				syslog.Warn("remote identity migration: provision failed")
				return false
			}
		}
		if reply, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminAddAccount, Name: []byte(plan.Name)}); !ok || reply.Result != identityproto.ResultOK {
			syslog.Warn("remote identity migration: account creation failed")
			return false
		}
		if reply, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminSetHome, Name: []byte(plan.Name), Home: []byte(plan.Home)}); !ok || reply.Result != identityproto.ResultOK {
			syslog.Warn("remote identity migration: home assignment failed")
			return false
		}
	case identityproto.ResultOK:
		if len(account.Records) != 1 || string(account.Records[0].Home) != plan.Home {
			syslog.Warn("remote identity migration: account exists with a conflicting name or home; resolution required")
			return false
		}
	default:
		syslog.Warn("remote identity migration: account lookup failed", syslog.Uint("result", uint64(account.Result)))
		return false
	}
	for _, key := range plan.Keys {
		reply, ok := identityAdminCall(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte(plan.Name), Key: key})
		if !ok || reply.Result != identityproto.ResultOK && reply.Result != identityproto.ResultExists {
			syslog.Warn("remote identity migration: key import failed")
			return false
		}
	}
	syslog.Info("remote identity migration committed", syslog.Str("user", plan.Name))
	return true
}
