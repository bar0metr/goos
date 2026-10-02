package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Artifact is one input file as it was uploaded.
type Artifact struct {
	Path     string    `json:"path"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type Artifacts struct {
	ISO  Artifact `json:"iso"`
	Disk Artifact `json:"disk"`
}

func describeArtifact(root, path string) (Artifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return Artifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Artifact{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Artifact{}, fmt.Errorf("%s: not a non-empty regular file", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return Artifact{}, err
	}
	shown := path
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		shown = rel
	}
	return Artifact{Path: shown, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size(), Modified: info.ModTime().UTC()}, nil
}

// developerDisk is the mutable disk make run boots; a lab run never uses it.
func checkNotDeveloperDisk(root, disk string) error {
	abs, err := filepath.Abs(disk)
	if err != nil {
		return err
	}
	if abs == filepath.Join(root, "build", "goos-disk.img") {
		return fmt.Errorf("%s is the developer's run disk; build fresh lab inputs with make pve-artifacts", disk)
	}
	return nil
}

func ManifestArtifacts(root, iso, disk string) (Artifacts, error) {
	if err := checkNotDeveloperDisk(root, disk); err != nil {
		return Artifacts{}, err
	}
	var a Artifacts
	var err error
	if a.ISO, err = describeArtifact(root, iso); err != nil {
		return a, fmt.Errorf("ISO: %w", err)
	}
	if a.Disk, err = describeArtifact(root, disk); err != nil {
		return a, fmt.Errorf("disk: %w", err)
	}
	return a, nil
}

type GitInfo struct {
	SHA   string `json:"sha"`
	Dirty bool   `json:"dirty"`
}

func (g GitInfo) String() string {
	short := g.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	if g.Dirty {
		return short + " (dirty)"
	}
	return short
}

func ReadGit(root string) (GitInfo, error) {
	sha, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return GitInfo{}, err
	}
	status, err := gitOutput(root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return GitInfo{}, err
	}
	return GitInfo{SHA: sha, Dirty: status != ""}, nil
}

func gitOutput(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
