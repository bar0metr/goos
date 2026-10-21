//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestFilesystemFenceStopsStalledServiceThenFencesItsReplacement(t *testing.T) {
	var fence filesystemFence
	if !fence.launched(10) {
		t.Fatal("the first instance has no predecessor to fence")
	}
	generation := fence.attaching()
	if action := fence.lost(generation); action != filesystemLostStop {
		t.Fatalf("live stalled instance: action %d", action)
	}
	if action := fence.lost(generation); action != filesystemLostIgnored {
		t.Fatal("one notice acted on twice")
	}
	fence.exited(10)
	if !fence.launched(11) {
		t.Fatal("replacement after the reported exit not fenced")
	}
	if replacement := fence.attaching(); replacement == generation {
		t.Fatal("replacement reused the lost generation")
	}
}

func TestFilesystemFenceRequiresTheReportedExit(t *testing.T) {
	var fence filesystemFence
	fence.launched(10)
	fence.lost(fence.attaching())
	// The old writer's late cache writes are possible until the kernel
	// reports its exit. Another process's exit is no fence for it.
	fence.exited(12)
	if fence.launched(11) {
		t.Fatal("attach fenced while the old writer may still run")
	}
	fence.exited(10)
	fence.exited(11)
	if !fence.launched(13) {
		t.Fatal("attach after every exit not fenced")
	}
}

func TestFilesystemFenceIgnoresNoticeForReplacedAttachment(t *testing.T) {
	var fence filesystemFence
	fence.launched(10)
	old := fence.attaching()
	// The service died on its own; its replacement attached before the
	// VFS's notice about the old attachment was read.
	fence.exited(10)
	fence.launched(11)
	fence.attaching()
	if action := fence.lost(old); action != filesystemLostIgnored {
		t.Fatalf("stale notice would stop the healthy replacement: action %d", action)
	}
	if action := fence.lost(0); action != filesystemLostIgnored {
		t.Fatal("notice without a generation acted on")
	}
}

func TestFilesystemFenceAwaitsServiceThatAlreadyExited(t *testing.T) {
	var fence filesystemFence
	fence.launched(10)
	generation := fence.attaching()
	fence.exited(10)
	if action := fence.lost(generation); action != filesystemLostAwait {
		t.Fatalf("exited instance: action %d", action)
	}
}

func TestFilesystemFenceStopsAtTheBound(t *testing.T) {
	var fence filesystemFence
	pid := uint64(10)
	fence.launched(pid)
	for recovery := 0; recovery < filesystemRecoveryLimit; recovery++ {
		if action := fence.lost(fence.attaching()); action != filesystemLostStop {
			t.Fatalf("recovery %d: action %d", recovery, action)
		}
		fence.exited(pid)
		pid++
		if !fence.launched(pid) {
			t.Fatalf("recovery %d: replacement not fenced", recovery)
		}
	}
	generation := fence.attaching()
	if action := fence.lost(generation); action != filesystemLostExhausted {
		t.Fatalf("failure past the bound: action %d", action)
	}
	if action := fence.lost(fence.attaching()); action != filesystemLostIgnored {
		t.Fatal("recovery resumed after the bound")
	}
	// The cache stays closed: no later attach in this boot is fenced, even
	// after the stalled instance exits on its own.
	fence.exited(pid)
	if fence.launched(pid + 1) {
		t.Fatal("attach fenced after the bound")
	}
}

// The fence is only as good as its inputs: every attach takes its fence from
// the launch it follows, and every reported exit reaches the fence.
func TestFilesystemFenceSeesLaunchesAndExits(t *testing.T) {
	calls := func(path, receiver, method string) []*ast.CallExpr {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var found []*ast.CallExpr
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == method {
				if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == receiver {
					found = append(found, call)
				}
			} else if name, ok := call.Fun.(*ast.Ident); ok && receiver == "" && name.Name == method {
				found = append(found, call)
			}
			return true
		})
		return found
	}
	attaches := calls("launch.go", "", "attachFilesystem")
	if len(attaches) != 1 || len(attaches[0].Args) != 1 {
		t.Fatalf("%d filesystem attaches in launch.go, want 1", len(attaches))
	}
	argument, ok := attaches[0].Args[0].(*ast.CallExpr)
	if !ok {
		t.Fatal("attach fence is not taken from the launch")
	}
	if selector, ok := argument.Fun.(*ast.SelectorExpr); !ok || selector.Sel.Name != "launched" {
		t.Fatal("attach fence is not taken from the launch")
	}
	if len(calls("main.go", "cacheFence", "exited")) != 1 {
		t.Fatal("the main loop does not report exits to the fence")
	}
}
