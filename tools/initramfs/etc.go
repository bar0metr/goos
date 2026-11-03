package main

// The archive's etc directory, which the VFS mounts read-only at /etc
// (docs/architecture/filesystem.md). A static C library reads these files
// itself: it finds the resolver's local DNS server on 127.0.0.1:53 through
// resolv.conf, and looks in hosts before it asks DNS. The hosts table that
// native commands use lives in the configuration service, not here.

const (
	resolvConf   = "nameserver 127.0.0.1\n"
	etcHosts     = "127.0.0.1 localhost\n"
	nsswitchConf = "hosts: files dns\n"
)

func etc() []member {
	return []member{
		directory("system/etc/"),
		file("system/etc/hosts", 0o444, []byte(etcHosts)),
		file("system/etc/nsswitch.conf", 0o444, []byte(nsswitchConf)),
		file("system/etc/resolv.conf", 0o444, []byte(resolvConf)),
	}
}
