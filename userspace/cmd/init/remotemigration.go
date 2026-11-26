//go:build goos_userspace

package main

import (
	configproto "goos/userspace/config"
)

// legacyRemoteAccount returns the account name the legacy system.remote.user
// setting names. The legacy key-mirror path derives the one imported account
// from it, so no process-local migration state survives a restart. The legacy
// import itself is an explicit local command, never an incoming connection.
func legacyRemoteAccount() ([]byte, bool) {
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		return nil, false
	}
	defer closeOpen(settings)
	user, ok := remoteConfigText(settings, "system.remote.user")
	if !ok {
		return nil, false
	}
	return []byte(user), true
}
