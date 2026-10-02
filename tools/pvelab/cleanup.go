package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errNotOwned stops cleanup: the VM the journal names no longer carries
// this run's marker, and only a person can decide what it is.
var errNotOwned = errors.New("ownership check failed; not touching the VM")

// ownershipProblems lists why config does not belong to the run, or nil.
// The marker in the description, the tag, the name, and the pool must all
// agree with the journal.
func ownershipProblems(st *State, config map[string]string, pool string) []string {
	var problems []string
	desc := map[string]bool{}
	for _, line := range strings.Split(config["description"], "\n") {
		desc[strings.TrimSpace(line)] = true
	}
	if !desc[ownerMarker] {
		problems = append(problems, "description lacks "+ownerMarker)
	}
	if !desc["run-id="+st.RunID] {
		problems = append(problems, "description names another run")
	}
	if !hasTag(config["tags"], ownerTag) {
		problems = append(problems, "tag "+ownerTag+" missing")
	}
	if st.VMName != "" && config["name"] != st.VMName {
		problems = append(problems, fmt.Sprintf("name is %q, want %q", config["name"], st.VMName))
	}
	if pool != st.Pool {
		problems = append(problems, fmt.Sprintf("pool is %q, want %q", pool, st.Pool))
	}
	return problems
}

func hasTag(tags, want string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t == want {
			return true
		}
	}
	return false
}

// Cleanup removes everything the journal records, in order: the VM (after
// the ownership check), its disks, the ISO, and the staging directory. A
// resource already gone counts as removed, so Cleanup can run again.
func (l *Lab) Cleanup(ctx context.Context, st *State) error {
	st.Advance(PhaseCleaning)
	var errs []error
	if st.VMID != 0 {
		if err := l.destroyVM(ctx, st); err != nil {
			if errors.Is(err, errNotOwned) {
				// Stop here: the ISO and staging may be in use by
				// whatever the VM now is.
				st.Save()
				return err
			}
			errs = append(errs, err)
		}
	}
	if st.ISOVolume != "" {
		if err := l.deleteISO(ctx, st); err != nil {
			errs = append(errs, err)
		}
	}
	if st.RemoteStaging != "" {
		if err := l.ssh.RemoveStaging(ctx, st.RemoteStaging, st.RunID); err != nil {
			errs = append(errs, fmt.Errorf("staging: %w", err))
		} else {
			l.log.Debugf("staging removed")
			st.RemoteStaging = ""
			st.Save()
		}
	}
	if len(errs) > 0 {
		st.Save()
		return errors.Join(errs...)
	}
	st.Cleaned = true
	return st.Advance(PhaseDone)
}

func (l *Lab) destroyVM(ctx context.Context, st *State) error {
	// Proxmox answers 403, not 404, for the configuration of a VMID that
	// does not exist, because no pool ACL covers it. The cluster resource
	// list shows exactly the VMs the token can see, so it decides whether
	// the VM is there. A VM the token cannot see is outside the pool and
	// not pvelab's to touch.
	vm, err := l.api.ClusterVM(ctx, st.VMID)
	if err != nil {
		return fmt.Errorf("vm %d: %w", st.VMID, err)
	}
	if vm == nil {
		l.log.Debugf("vm %d already gone", st.VMID)
		return l.forgetVM(ctx, st)
	}
	config, err := l.api.VMConfig(ctx, st.VMID)
	if err != nil {
		return fmt.Errorf("vm %d: %w", st.VMID, err)
	}
	if problems := ownershipProblems(st, config, vm.Pool); len(problems) > 0 {
		return fmt.Errorf("vm %d: %w: %s", st.VMID, errNotOwned, strings.Join(problems, "; "))
	}

	status, err := l.api.VMStatus(ctx, st.VMID)
	if err != nil {
		return fmt.Errorf("vm %d status: %w", st.VMID, err)
	}
	if status.Status != "stopped" {
		upid, err := l.api.VMAction(ctx, st.VMID, "stop")
		if err == nil {
			err = l.api.WaitTask(ctx, upid)
		}
		if err != nil {
			return fmt.Errorf("stopping vm %d: %w", st.VMID, err)
		}
		l.log.Debugf("vm stopped")
	}
	upid, err := l.api.DestroyVM(ctx, st.VMID)
	if err == nil {
		err = l.api.WaitTask(ctx, upid)
	}
	if err != nil && !isMissing(err) {
		return fmt.Errorf("destroying vm %d: %w", st.VMID, err)
	}
	if vm, err := l.api.ClusterVM(ctx, st.VMID); err != nil || vm != nil {
		if err == nil {
			err = errors.New("still exists after destroy")
		}
		return fmt.Errorf("vm %d: %w", st.VMID, err)
	}
	l.log.Infof("vm destroyed")
	return l.forgetVM(ctx, st)
}

// forgetVM checks that the VM's disks went with it and clears the journal.
func (l *Lab) forgetVM(ctx context.Context, st *State) error {
	for _, vol := range st.DiskVolumes {
		storage, _, _ := strings.Cut(vol, ":")
		present, err := l.volumePresent(ctx, storage, "images", vol)
		if err != nil {
			return fmt.Errorf("checking %s: %w", vol, err)
		}
		if present {
			return fmt.Errorf("%s is still present after the VM was destroyed", vol)
		}
	}
	st.VMID, st.VMCreated, st.VMStarted, st.DiskVolumes = 0, false, false, nil
	return st.Save()
}

func (l *Lab) deleteISO(ctx context.Context, st *State) error {
	upid, err := l.api.DeleteVolume(ctx, st.ISOStorage, st.ISOVolume)
	if err == nil {
		err = l.api.WaitTask(ctx, upid)
	}
	if err != nil && !isMissing(err) {
		return fmt.Errorf("deleting %s: %w", st.ISOVolume, err)
	}
	present, err := l.volumePresent(ctx, st.ISOStorage, "iso", st.ISOVolume)
	if err != nil {
		return fmt.Errorf("checking %s: %w", st.ISOVolume, err)
	}
	if present {
		return fmt.Errorf("%s is still present after delete", st.ISOVolume)
	}
	l.log.Infof("iso removed")
	st.ISOVolume, st.ISOUploadStarted = "", false
	return st.Save()
}

// Destroy cleans a run from its saved journal.
func (l *Lab) Destroy(ctx context.Context, runID string) int {
	st, err := LoadState(l.cfg.RepoRoot, runID)
	if err != nil {
		l.log.Infof("error: %v", err)
		return ExitConfig
	}
	if st.Node != l.cfg.Node || st.Pool != l.cfg.Pool {
		l.log.Infof("error: run %s was made on node %s pool %s; the configuration names node %s pool %s",
			runID, st.Node, st.Pool, l.cfg.Node, l.cfg.Pool)
		return ExitConfig
	}
	if st.Cleaned {
		l.log.Infof("run %s is already cleaned", runID)
		return ExitPass
	}
	l.log.Infof("destroy run %s (vmid %d)", runID, st.VMID)
	cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := l.Cleanup(cctx, st); err != nil {
		l.log.Infof("cleanup FAILED: %v", err)
		return ExitCleanup
	}
	l.log.Infof("cleanup PASS")
	return ExitPass
}

// staleRuns lists the journals of runs not cleaned and older than age.
func staleRuns(root string, age time.Duration, now time.Time) ([]*State, error) {
	entries, err := os.ReadDir(runsDir(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stale []*State
	for _, e := range entries {
		if !e.IsDir() || !runIDPattern.MatchString(e.Name()) {
			continue
		}
		st, err := LoadState(root, e.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !st.Cleaned && now.Sub(st.UpdatedAt) >= age {
			stale = append(stale, st)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].RunID < stale[j].RunID })
	return stale, nil
}

// CleanupStale destroys the resources of old, uncleaned runs. It considers
// local journals only, never VMs discovered on the host.
func (l *Lab) CleanupStale(ctx context.Context, age time.Duration, yes bool) int {
	stale, err := staleRuns(l.cfg.RepoRoot, age, time.Now())
	if err != nil {
		l.log.Infof("error: %v", err)
		return ExitConfig
	}
	var mine []*State
	for _, st := range stale {
		if st.Node == l.cfg.Node && st.Pool == l.cfg.Pool {
			mine = append(mine, st)
		}
	}
	if len(mine) == 0 {
		l.log.Infof("no uncleaned runs older than %s", age)
		return ExitPass
	}
	for _, st := range mine {
		var what []string
		if st.VMID != 0 {
			what = append(what, "vm "+strconv.Itoa(st.VMID))
		}
		if st.ISOVolume != "" {
			what = append(what, st.ISOVolume)
		}
		if st.RemoteStaging != "" {
			what = append(what, st.RemoteStaging)
		}
		if len(what) == 0 {
			what = append(what, "journal only")
		}
		l.log.Infof("plan: run %s (%s, updated %s): %s", st.RunID, st.Phase, st.UpdatedAt.Format(time.RFC3339), strings.Join(what, ", "))
	}
	if !yes {
		if !isTerminal(os.Stdin) {
			l.log.Infof("error: not a terminal; pass --yes to clean without asking")
			return ExitConfig
		}
		fmt.Fprint(l.log.w, "[pvelab] clean these runs? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(answer)); a != "y" && a != "yes" {
			l.log.Infof("nothing done")
			return ExitPass
		}
	}
	code := ExitPass
	for _, st := range mine {
		cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		err := l.Cleanup(cctx, st)
		cancel()
		if err != nil {
			l.log.Infof("run %s: cleanup FAILED: %v", st.RunID, err)
			code = ExitCleanup
			continue
		}
		l.log.Infof("run %s cleaned", st.RunID)
	}
	return code
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
