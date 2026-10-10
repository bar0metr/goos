//go:build goos_userspace

// Command trust verifies and installs independently signed system CA updates.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/cryptosys"
	"goos/userspace/fetchtransport"
	"goos/userspace/nativefiles"
	"goos/userspace/runtime/arena"
	"goos/userspace/startup"
	"goos/userspace/tlsclient"
	"goos/userspace/truststate"
	"goos/userspace/truststore"
	"goos/userspace/trustupdate"
	"goos/userspace/utility"
	"strconv"
	"strings"
)

var manifest = commands.Manifest{
	Usage:   "trust status | verify FILE META SIG | install FILE META SIG | update",
	Summary: "verify and install signed system trust updates",
	MinArgs: 1, MaxArgs: 4,
	Grants: commands.GrantResolve | commands.GrantTCP | commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { arena.InitPackages(); utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	var bulk utility.Bulk
	files := &nativefiles.Files{Bulk: &bulk}
	action, _ := data.Arg(1)
	if string(action) == "status" && data.ArgCount() == 1 {
		status(&bulk, files)
		return
	}
	valid := (string(action) == "verify" || string(action) == "install") && data.ArgCount() == 4 || string(action) == "update" && data.ArgCount() == 1
	if !valid {
		utility.Usage()
	}

	key, err := files.Read(truststate.KeyPath, ed25519.PublicKeySize)
	if err != nil || len(key) != ed25519.PublicKeySize {
		fail(errors.New("immutable trust update key unavailable"))
	}
	store := truststate.Store{Files: files, Key: key, Validate: validateRoots}
	var pkg *truststore.Package
	if string(action) == "update" {
		url, err := files.Read(truststate.URLPath, 4096)
		if err != nil {
			fail(err)
		}
		base := strings.TrimSuffix(string(url), "\n")
		if base == "" {
			fail(errors.New("trust update URL not configured in system image"))
		}
		transport, err := fetchtransport.New(data)
		if err != nil {
			fail(err)
		}
		bundle, meta, sig, err := trustupdate.Download(transport, base)
		if err != nil {
			fail(err)
		}
		pkg, err = truststore.Verify(bundle, meta, sig, key, validateRoots)
		if err != nil {
			fail(err)
		}
		utility.WriteError([]byte("trust: update available sequence=" + strconv.FormatUint(pkg.Metadata.Sequence, 10) + "\n"))
	} else {
		var paths [3]string
		for i := range paths {
			arg, _ := data.Arg(i + 2)
			paths[i] = string(utility.Resolve(data, arg))
		}
		pkg, err = store.VerifyFiles(paths[0], paths[1], paths[2])
		if err != nil {
			fail(err)
		}
	}
	if string(action) == "verify" {
		utility.PrintText("Trust package verified: sequence ")
		utility.PrintUint(pkg.Metadata.Sequence)
		utility.EndLine()
		return
	}
	info, _, status := utility.FilesystemInfo([]byte("/state"))
	if status != vfs.StatusOK || info.Type != ramfs.FilesystemFAT32 || info.ReadOnly {
		fail(errors.New("trust installation requires writable persistent FAT32 state"))
	}
	var nonce [8]byte
	if _, err := (cryptosys.Reader{}).Read(nonce[:]); err != nil {
		fail(err)
	}
	installed, err := store.Install(pkg, hex.EncodeToString(nonce[:]))
	if err != nil {
		fail(err)
	}
	utility.PrintText("Trust store updated: sequence ")
	utility.PrintUint(installed.Metadata.Sequence)
	utility.EndLine()
	utility.WriteError([]byte("trust: installed sequence=" + strconv.FormatUint(installed.Metadata.Sequence, 10) + " roots=" + strconv.Itoa(installed.Metadata.Certificates) + "\n"))
}

func validateRoots(data []byte) (int, error) {
	roots, err := tlsclient.LoadRoots(data)
	if err != nil {
		return 0, err
	}
	return roots.Count(), nil
}
func fail(err error) { utility.FailStatus(1, nil, "trust: "+err.Error()) }

func status(bulk *utility.Bulk, files *nativefiles.Files) {
	roots, selected, err := tlsclient.LoadSelectedRoots(bulk)
	if err != nil {
		fail(err)
	}
	if selected.Package == nil {
		utility.PrintText("Trust store: system fallback\nReason: ")
		utility.PrintText(selected.Reason)
		utility.EndLine()
		utility.PrintText("Roots: ")
		utility.PrintUint(uint64(roots.Count()))
		utility.EndLine()
		utility.PrintText("Path: ")
		utility.PrintText(tlsclient.RootPath)
		utility.EndLine()
		return
	}
	meta := selected.Package.Metadata
	utility.PrintText("Trust store: updated\nSequence: ")
	utility.PrintUint(meta.Sequence)
	utility.EndLine()
	utility.PrintText("Source: Mozilla\nSource date: ")
	utility.PrintText(meta.SourceDate)
	utility.EndLine()
	utility.PrintText("Roots: ")
	utility.PrintUint(uint64(meta.Certificates))
	utility.EndLine()
	utility.PrintText("Bundle SHA256: ")
	utility.PrintText(meta.BundleSHA256)
	utility.EndLine()
	utility.PrintText("Path: ")
	utility.PrintText(selected.Path)
	utility.EndLine()
	fallback, err := files.Read(tlsclient.RootPath, tlsclient.MaxBundle)
	if err != nil {
		fail(err)
	}
	count, err := validateRoots(fallback)
	if err != nil {
		fail(err)
	}
	utility.PrintText("Fallback roots: ")
	utility.PrintUint(uint64(count))
	utility.EndLine()
}
