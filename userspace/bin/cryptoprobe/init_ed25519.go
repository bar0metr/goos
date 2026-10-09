//go:build goos_userspace && cryptoprobe_ed25519

package main

import "goos/userspace/runtime/arena"

func initCryptoPackages() { arena.InitPackages() }
