//go:build !goos_userspace

package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func remoteFunctionSource(t *testing.T, path, name string) string {
	t.Helper()
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			var text bytes.Buffer
			if err := format.Node(&text, set, fn); err != nil {
				t.Fatal(err)
			}
			return text.String()
		}
	}
	t.Fatal("missing function", name)
	return ""
}

func TestRemoteSignerPairAndGrant(t *testing.T) {
	source := remoteFunctionSource(t, "remote.go", "launchRemoteWorker")
	for _, required := range []string{"createChannel(&signer)", "defer closeOpen(handle.ID(signer[0]))", "defer closeOpen(handle.ID(signer[1]))", "Name: \"signer\", Handle: handle.ID(signer[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true", "[]handle.ID{handle.ID(input[1]), handle.ID(output[0]), handle.ID(signer[0])}"} {
		if !strings.Contains(source, required) {
			t.Fatal("missing signer ownership contract", required)
		}
	}
}

func TestRemoteBootstrapHasNoFileGrants(t *testing.T) {
	source := remoteFunctionSource(t, "remotestorage.go", "openRemoteAuthority")
	if !strings.Contains(source, "openRemoteAuthoritySession(vfs.Authority{Confined: true})") || strings.Contains(source, "remotePaths(") {
		t.Fatal("bootstrap depends on invalid candidate or grants files")
	}
	protection := remoteFunctionSource(t, "remotestorage.go", "protectRemoteStorage")
	if !strings.Contains(protection, "protectConfiguredRemoteHost(files, host)") {
		t.Fatal("configured identity unprotected during degraded boot")
	}
	authority := remoteFunctionSource(t, "remotestorage.go", "remoteAuthority")
	if !strings.Contains(authority, "ensureRemotePolicyScopes(") || !strings.Contains(authority, "openRemoteConfinedAuthority()") {
		t.Fatal("candidate grant lacks scope preparation")
	}
}

func TestRemoteShellCannotReceiveHostSigner(t *testing.T) {
	source := remoteFunctionSource(t, "remote.go", "launchRemoteShell")
	for _, name := range []string{"signer", "policy", "tcp", "console"} {
		if strings.Contains(source, "Name: \""+name+"\"") {
			t.Fatal("remote shell received worker authority", name)
		}
	}
}
