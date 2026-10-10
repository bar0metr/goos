package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"testing"
)

type authorityFaultTransport struct {
	inner               Transport
	mutationError       bool
	lookupAfterMutation bool
	lookupArmed         bool
	infoError           bool
}

func (transport *authorityFaultTransport) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if transport.infoError && request.Operation == fsproto.OperationFilesystemInfo {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	if transport.lookupArmed && request.Operation == fsproto.OperationOpen {
		transport.lookupArmed = false
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	reply, status := transport.inner.Exchange(request)
	if status == ramfs.StatusOK && reply.Status == ramfs.StatusOK && (request.Operation == fsproto.OperationRename || request.Operation == fsproto.OperationReplace) {
		if transport.mutationError {
			return fsproto.Reply{}, ramfs.StatusIOError
		}
		transport.lookupArmed = transport.lookupAfterMutation
	}
	return reply, status
}

func TestAmbiguousScopedMovesRevokeOldFATObjects(t *testing.T) {
	for _, operation := range []vfs.Operation{vfs.OperationRename, vfs.OperationReplace} {
		for _, commitError := range []bool{true, false} {
			t.Run(operationName(operation, commitError), func(t *testing.T) {
				service, _ := protectedRemote(t)
				// This trusted session may move an ordinary source into the private scope.
				authority := vfs.Authority{}
				authority.Grants[0] = vfs.Read | vfs.Write
				if service.SetAuthority(remoteOwner, authority) != vfs.StatusOK {
					t.Fatal("authority")
				}
				source := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/state/public"), Rights: vfs.Read | vfs.Write}).Handle
				original := service.persistent.transport
				fault := &authorityFaultTransport{inner: original, mutationError: commitError, lookupAfterMutation: !commitError}
				service.persistent.transport = fault
				target := "/state/security/ssh/moved"
				if operation == vfs.OperationReplace {
					target = "/state/security/ssh/host"
				}
				reply := service.Dispatch(remoteOwner, vfs.Request{Operation: operation, Path: []byte("/state/public"), Target: []byte(target)})
				if commitError && reply.Status == vfs.StatusOK {
					t.Fatal("lost failure")
				}
				if !commitError && reply.Status != vfs.StatusOK {
					t.Fatalf("move=%d", reply.Status)
				}
				service.persistent.transport = original
				var bytes [64]byte
				if _, status := service.ReadFile(testSession, source, bytes[:]); status != vfs.StatusNotFound {
					t.Fatalf("ambiguous read=%d", status)
				}
				if _, status := service.ReadFileAt(testSession, source, 0, bytes[:]); status != vfs.StatusNotFound {
					t.Fatalf("ambiguous readat=%d", status)
				}
				if _, status := service.WriteFile(testSession, source, []byte("corrupt")); status != vfs.StatusNotFound {
					t.Fatalf("ambiguous write=%d", status)
				}
				if status := service.PrepareExecutable(testSession, source); status != vfs.StatusNotFound {
					t.Fatalf("ambiguous prepare=%d", status)
				}
				if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/state/public")}); reply.Status != vfs.StatusNotFound {
					t.Fatalf("stale cached source=%d", reply.Status)
				}
				deny(t, service, testSession, vfs.Request{Operation: vfs.OperationOpen, Path: []byte(target), Rights: vfs.Read})
			})
		}
	}
}

func operationName(operation vfs.Operation, commitError bool) string {
	name := "rename"
	if operation == vfs.OperationReplace {
		name = "replace"
	}
	if commitError {
		return name + "-commit-error"
	}
	return name + "-lookup-error"
}

func TestExactScopeRequiresKnownFATNameSemantics(t *testing.T) {
	service := &Service{}
	if !service.Init() || !service.AttachCache(testCache) {
		t.Fatal("init")
	}
	fault := &authorityFaultTransport{inner: newLoopback(testFAT32Device()), infoError: true}
	if !service.AttachFilesystem(fault) {
		t.Fatal("attach")
	}
	if status := service.InstallScope(0, []byte("/state/HostKey"), false, vfs.Read|vfs.Write); status != vfs.StatusPermissionDenied {
		t.Fatalf("unknown semantics install=%d", status)
	}
	if service.namespace.AuthorizePath(remoteOwner, []byte("/state/HOSTKEY"), vfs.Write, false) != vfs.StatusOK {
		t.Fatal("failed install left partial scope")
	}
	// A later alias has known semantics, but the old aliases remain unknown.
	fault.infoError = false
	_, state, status := service.namespace.Resolve([]byte("/state"))
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	if service.namespace.Mount([]byte("/known-state"), &service.persistent, state) != vfs.StatusOK {
		t.Fatal("mount")
	}
	scopeOK(t, service, 0, "/known-state/HostKey", false, vfs.Read|vfs.Write)
	for _, path := range []string{"/state/HOSTKEY", "/persist/state/hostkey"} {
		deny(t, service, testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)}, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(path)})
	}
}
