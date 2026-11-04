package main

import (
	"bytes"
	"context"
	"fmt"
	"path"
)

// labNode is the NodeAccess of a run: commands go through the run's SSH
// settings, and staged files land in stagingDir(runID), which the run's
// state records so that cleanup and destroy remove it.
type labNode struct {
	ssh SSH
	st  *State
	dir string
}

func (n *labNode) Run(ctx context.Context, argv ...string) ([]byte, error) {
	return n.ssh.Run(ctx, argv...)
}

// Shell runs script through the node's login shell and returns its output.
func (n *labNode) Shell(ctx context.Context, script string) ([]byte, error) {
	cmd := n.ssh.Command(ctx, script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, lastLines(stderr.String()+stdout.String(), 8))
	}
	return stdout.Bytes(), nil
}

// Stage writes data to name in the run's staging directory, making the
// directory first.
func (n *labNode) Stage(ctx context.Context, name string, data []byte) (string, error) {
	if name == "" || path.Base(name) != name {
		return "", fmt.Errorf("refusing to stage %q", name)
	}
	if n.dir == "" {
		dir, err := n.ssh.MakeStaging(ctx, n.st.RunID)
		if err != nil {
			return "", err
		}
		n.dir = dir
		n.st.RemoteStaging = dir
		if err := n.st.Save(); err != nil {
			return "", err
		}
	}
	file := n.dir + "/" + name
	cmd := n.ssh.Command(ctx, "umask 077 && cat > "+shellQuote(file))
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("staging %s: %w: %s", file, err, lastLines(string(out), 4))
	}
	return file, nil
}

// Unstage stops what the staging directory's pid file names and removes the
// directory.
func (n *labNode) Unstage(ctx context.Context) error {
	if n.dir == "" {
		return nil
	}
	pid := shellQuote(n.dir + "/peer.pid")
	n.Shell(ctx, "test -f "+pid+" && kill $(cat "+pid+") 2>/dev/null; true")
	if err := n.ssh.RemoveStaging(ctx, n.dir, n.st.RunID); err != nil {
		return err
	}
	n.dir = ""
	n.st.RemoteStaging = ""
	return n.st.Save()
}
