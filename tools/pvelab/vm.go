package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stage timeouts. Each stage has its own, so an error names the stage that
// ran out of time.
const (
	taskTimeout    = 120 * time.Second
	uploadTimeout  = 300 * time.Second
	importTimeout  = 300 * time.Second
	cleanupTimeout = 120 * time.Second
)

// ownerTag and the description lines mark a VM as made by pvelab. Cleanup
// checks them before it destroys anything.
const (
	ownerTag    = "goos-pvelab"
	ownerMarker = "managed-by=goos-pvelab"
)

func ownershipDescription(runID, sha string) string {
	return ownerMarker + "\nrun-id=" + runID + "\ngit-sha=" + sha + "\n"
}

// Logger prints the concise run log; Debugf prints only with --verbose.
type Logger struct {
	w       io.Writer
	verbose bool
}

func (l *Logger) Infof(format string, args ...any) {
	fmt.Fprintf(l.w, "[pvelab] "+format+"\n", args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if l.verbose {
		fmt.Fprintf(l.w, "[pvelab]   "+format+"\n", args...)
	}
}

// Lab is one configured connection to the Proxmox node.
type Lab struct {
	cfg *Config
	api *API
	ssh SSH
	log *Logger
}

func NewLab(cfg *Config, log *Logger) (*Lab, error) {
	api, err := NewAPI(cfg, log.Debugf)
	if err != nil {
		return nil, fail(ClassConfig, "config", err)
	}
	return &Lab{cfg: cfg, api: api, ssh: NewSSH(cfg, log.Debugf), log: log}, nil
}

func stageCtx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// progress returns a callback that logs every quarter of total in verbose
// mode.
func (l *Lab) progress(what string, total int64) func(int64) {
	next := total / 4
	return func(sent int64) {
		if total > 0 && sent >= next {
			l.log.Debugf("%s %d MiB / %d MiB", what, sent>>20, total>>20)
			next += total / 4
		}
	}
}

// CreateVM allocates a VMID and creates the VM with its ownership marker,
// firmware, and EFI disk. The ISO and system disk come later.
func (l *Lab) CreateVM(ctx context.Context, st *State) error {
	for attempt := 0; attempt < 3; attempt++ {
		vmid, err := l.api.NextID(ctx)
		if err != nil {
			return fail(ClassInfra, "vmid", err)
		}
		st.VMID, st.VMName = vmid, vmName(st.RunID)
		if err := st.Save(); err != nil {
			return fail(ClassInfra, "state", err)
		}
		c := l.cfg
		form := url.Values{
			"vmid":        {strconv.Itoa(vmid)},
			"name":        {st.VMName},
			"pool":        {c.Pool},
			"tags":        {ownerTag},
			"description": {ownershipDescription(st.RunID, st.GitSHA)},
			"machine":     {c.Machine},
			"bios":        {c.BIOS},
			"cpu":         {c.CPU},
			"sockets":     {strconv.Itoa(c.Sockets)},
			"cores":       {strconv.Itoa(c.Cores)},
			"memory":      {strconv.Itoa(c.Memory)},
			"balloon":     {"0"},
			"ostype":      {"other"},
			"serial0":     {"socket"},
			"tablet":      {"0"},
			"onboot":      {"0"},
		}
		if strings.EqualFold(c.BIOS, "ovmf") {
			// No pre-enrolled keys: GoOS does not support Secure Boot.
			form.Set("efidisk0", c.EFIStorage+":1,efitype=4m,pre-enrolled-keys=0")
		}
		upid, err := l.api.CreateVM(ctx, form)
		if err != nil {
			// Refused before anything was made; the VMID may now be
			// someone else's, so it must not stay in the journal.
			st.VMID, st.VMName = 0, ""
			st.Save()
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fail(ClassInfra, "create vm", err)
		}
		tctx, cancel := stageCtx(ctx, taskTimeout)
		err = l.api.WaitTask(tctx, upid)
		cancel()
		if err != nil {
			return fail(ClassInfra, "create vm", err)
		}
		st.VMCreated = true
		return fail(ClassInfra, "state", st.Advance(PhaseVMAllocated))
	}
	return failf(ClassInfra, "create vm", "three allocated VMIDs were taken before use")
}

// UploadISO uploads the boot ISO under a name unique to the run.
func (l *Lab) UploadISO(ctx context.Context, st *State, iso Artifact, path string) error {
	name := isoName(st.RunID)
	st.ISOStorage = l.cfg.ISOStorage
	st.ISOVolume = l.cfg.ISOStorage + ":iso/" + name
	st.ISOUploadStarted = true
	if err := st.Save(); err != nil {
		return fail(ClassInfra, "state", err)
	}
	uctx, cancel := stageCtx(ctx, uploadTimeout)
	defer cancel()
	upid, err := l.api.UploadISO(uctx, l.cfg.ISOStorage, name, path, iso.SHA256, l.progress("upload iso", iso.Size))
	if err != nil {
		return fail(ClassInfra, "upload iso", err)
	}
	if err := l.api.WaitTask(uctx, upid); err != nil {
		return fail(ClassInfra, "upload iso", err)
	}
	present, err := l.volumePresent(ctx, l.cfg.ISOStorage, "iso", st.ISOVolume)
	if err != nil {
		return fail(ClassInfra, "upload iso", err)
	}
	if !present {
		return failf(ClassInfra, "upload iso", "%s is not in the storage after the upload", st.ISOVolume)
	}
	return fail(ClassInfra, "state", st.Advance(PhaseISOUploaded))
}

func (l *Lab) volumePresent(ctx context.Context, storage, content, volid string) (bool, error) {
	vols, err := l.api.StorageContent(ctx, storage, content)
	if err != nil {
		return false, err
	}
	for _, v := range vols {
		if v.Volid == volid {
			return true, nil
		}
	}
	return false, nil
}

// ImportDisk stages the raw disk on the node, imports it into the disk
// storage as an unused volume of the VM, and removes the staged copy. The
// new volume is read from the VM configuration, not from qm's output.
func (l *Lab) ImportDisk(ctx context.Context, st *State, disk Artifact, path string) (string, error) {
	dir, err := l.ssh.MakeStaging(ctx, st.RunID)
	if err != nil {
		return "", fail(ClassInfra, "stage disk", err)
	}
	st.RemoteStaging = dir
	if err := st.Save(); err != nil {
		return "", fail(ClassInfra, "state", err)
	}
	remote := dir + "/goos-disk.img"
	ictx, cancel := stageCtx(ctx, importTimeout)
	defer cancel()
	if err := l.ssh.CopyTo(ictx, path, remote, l.progress("stage disk", disk.Size)); err != nil {
		return "", fail(ClassInfra, "stage disk", err)
	}
	sum, err := l.ssh.RemoteSHA256(ictx, remote)
	if err != nil {
		return "", fail(ClassInfra, "stage disk", err)
	}
	if sum != disk.SHA256 {
		return "", failf(ClassInfra, "stage disk", "staged disk has SHA-256 %s, want %s", sum, disk.SHA256)
	}

	before, err := l.api.VMConfig(ctx, st.VMID)
	if err != nil {
		return "", fail(ClassInfra, "import disk", err)
	}
	vmid := strconv.Itoa(st.VMID)
	_, err = l.ssh.Run(ictx, l.ssh.Root("qm", "disk", "import", vmid, remote, l.cfg.DiskStorage, "--format", "raw")...)
	if err != nil && strings.Contains(err.Error(), "unknown command") {
		// Proxmox before 7.2 has only the older spelling.
		_, err = l.ssh.Run(ictx, l.ssh.Root("qm", "importdisk", vmid, remote, l.cfg.DiskStorage, "--format", "raw")...)
	}
	if err != nil {
		return "", fail(ClassInfra, "import disk", err)
	}
	after, err := l.api.VMConfig(ctx, st.VMID)
	if err != nil {
		return "", fail(ClassInfra, "import disk", err)
	}
	volume, err := newUnusedVolume(before, after)
	if err != nil {
		return "", fail(ClassInfra, "import disk", err)
	}
	st.DiskVolumes = append(st.DiskVolumes, volume)
	if efi, ok := after["efidisk0"]; ok {
		st.DiskVolumes = append(st.DiskVolumes, volumeID(efi))
	}
	if err := l.ssh.RemoveStaging(ctx, dir, st.RunID); err != nil {
		return "", fail(ClassInfra, "stage disk", err)
	}
	st.RemoteStaging = ""
	return volume, fail(ClassInfra, "state", st.Advance(PhaseDiskImported))
}

// volumeID is the volume of a drive setting, without its options.
func volumeID(drive string) string {
	id, _, _ := strings.Cut(drive, ",")
	return id
}

// newUnusedVolume finds the one unusedN volume after has and before lacks.
func newUnusedVolume(before, after map[string]string) (string, error) {
	old := map[string]bool{}
	for k, v := range before {
		if strings.HasPrefix(k, "unused") {
			old[volumeID(v)] = true
		}
	}
	var found []string
	for k, v := range after {
		if strings.HasPrefix(k, "unused") && !old[volumeID(v)] {
			found = append(found, volumeID(v))
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errors.New("the import left no new unused volume in the VM configuration")
	}
	return "", fmt.Errorf("the import left several new volumes: %s", strings.Join(found, ", "))
}

// ConfigureVM attaches the system disk as virtio0 and the ISO as the only
// boot device, then checks the configuration Proxmox stored.
func (l *Lab) ConfigureVM(ctx context.Context, st *State, diskVolume string) (map[string]string, error) {
	form := url.Values{
		"virtio0": {diskVolume},
		"ide2":    {st.ISOVolume + ",media=cdrom"},
		"boot":    {"order=ide2"},
	}
	upid, err := l.api.SetVMConfig(ctx, st.VMID, form)
	if err != nil {
		return nil, fail(ClassInfra, "configure vm", err)
	}
	tctx, cancel := stageCtx(ctx, taskTimeout)
	defer cancel()
	if err := l.api.WaitTask(tctx, upid); err != nil {
		return nil, fail(ClassInfra, "configure vm", err)
	}
	config, err := l.api.VMConfig(ctx, st.VMID)
	if err != nil {
		return nil, fail(ClassInfra, "configure vm", err)
	}
	if volumeID(config["virtio0"]) != diskVolume {
		return nil, failf(ClassInfra, "configure vm", "virtio0 is %q, want %s", config["virtio0"], diskVolume)
	}
	if volumeID(config["ide2"]) != st.ISOVolume {
		return nil, failf(ClassInfra, "configure vm", "ide2 is %q, want %s", config["ide2"], st.ISOVolume)
	}
	return config, fail(ClassInfra, "state", st.Advance(PhaseVMConfigured))
}

func (l *Lab) StartVM(ctx context.Context, st *State) error {
	upid, err := l.api.VMAction(ctx, st.VMID, "start")
	if err != nil {
		return fail(ClassInfra, "start vm", err)
	}
	st.VMStarted = true
	st.Save()
	tctx, cancel := stageCtx(ctx, taskTimeout)
	defer cancel()
	if err := l.api.WaitTask(tctx, upid); err != nil {
		return fail(ClassInfra, "start vm", err)
	}
	return fail(ClassInfra, "state", st.Advance(PhaseVMStarted))
}

// HostInfo is what makes a hardware run comparable with another.
type HostInfo struct {
	ProxmoxVersion string     `json:"proxmox_version"`
	Node           string     `json:"node"`
	NodeStatus     NodeStatus `json:"node_status"`
	GuestCPU       string     `json:"guest_cpu"`
	KVM            bool       `json:"kvm"`
	PVEVersion     string     `json:"pveversion,omitempty"`
	Uname          string     `json:"uname,omitempty"`
	LSCPU          string     `json:"lscpu,omitempty"`
}

func (l *Lab) HostInfo(ctx context.Context, version string) HostInfo {
	info := HostInfo{ProxmoxVersion: version, Node: l.cfg.Node, GuestCPU: l.cfg.CPU}
	if s, err := l.api.NodeStatus(ctx); err == nil {
		info.NodeStatus = s
	}
	run := func(argv ...string) string {
		out, err := l.ssh.Run(ctx, argv...)
		if err != nil {
			return ""
		}
		text := strings.TrimSpace(string(out))
		if len(text) > 8192 {
			text = text[:8192]
		}
		return text
	}
	info.PVEVersion = run("pveversion")
	info.Uname = run("uname", "-a")
	info.LSCPU = run("lscpu")
	info.KVM = run("sh", "-c", "test -c /dev/kvm && echo yes") == "yes"
	return info
}

// RunOptions are the per-invocation settings of pvelab run.
type RunOptions struct {
	Scenario        string
	ISO             string
	Disk            string
	RequirePass     bool
	RequireClean    bool
	WorkloadTimeout time.Duration
}

// Run performs one disposable run and returns the exit status. Whatever
// happens after the run journal exists, the resources it names are either
// cleaned or, with --keep, reported for later destroy.
func (l *Lab) Run(ctx context.Context, opts RunOptions) int {
	scenario := scenarios[opts.Scenario]
	git, err := ReadGit(l.cfg.RepoRoot)
	if err != nil {
		l.log.Infof("error: git: %v", err)
		return ExitConfig
	}
	if opts.RequireClean && git.Dirty {
		l.log.Infof("error: the working tree has uncommitted changes and --require-clean is set")
		return ExitConfig
	}
	runID, err := NewRunID(time.Now())
	if err != nil {
		l.log.Infof("error: run ID: %v", err)
		return ExitInfra
	}
	st, err := NewState(l.cfg.RepoRoot, runID)
	if err != nil {
		l.log.Infof("error: state: %v", err)
		return ExitInfra
	}
	st.Node, st.Pool, st.GitSHA, st.Keep = l.cfg.Node, l.cfg.Pool, git.SHA, l.cfg.Keep
	st.Save()

	c := l.cfg
	report := &Report{
		Tool: "pvelab", RunID: runID, StartedAt: time.Now().UTC(), Git: git, Scenario: scenario.Name(),
		Proxmox: ProxmoxInfo{Node: c.Node, Pool: c.Pool},
		VM:      VMInfo{Machine: c.Machine, BIOS: c.BIOS, CPU: c.CPU, Sockets: c.Sockets, Cores: c.Cores, MemoryMiB: c.Memory},
	}
	l.log.Infof("run %s", runID)
	l.log.Infof("git %s", git)
	l.log.Infof("node %s", c.Node)
	if c.TLSInsecure {
		l.log.Infof("warning: TLS certificate verification is off (PVE_TLS_INSECURE=1)")
	}

	result, runErr := l.execute(ctx, st, report, opts, scenario)
	if runErr != nil && ctx.Err() != nil {
		stage := "run"
		var se *stageError
		if errors.As(runErr, &se) {
			stage = se.stage
		}
		runErr = failf(ClassInfra, stage, "interrupted")
	}

	if c.Keep {
		st.Kept = true
		st.Advance(PhaseKept)
		report.Kept = true
		report.Cleanup = "skipped: kept"
	} else {
		l.log.Infof("cleanup ...")
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		cleanupErr := l.Cleanup(cctx, st)
		cancel()
		if cleanupErr != nil {
			report.Cleanup = "failed: " + cleanupErr.Error()
			l.log.Infof("cleanup FAILED: %v", cleanupErr)
			l.log.Infof("retry with: go run ./tools/pvelab destroy --run %s", runID)
			if runErr == nil {
				runErr = fail(ClassCleanup, "cleanup", cleanupErr)
			}
		} else {
			report.Cleanup = "ok"
			l.log.Infof("cleanup PASS")
		}
	}

	code := ExitPass
	if runErr != nil {
		class := classOf(runErr)
		report.Result = string(class)
		report.Error = runErr.Error()
		var se *stageError
		if errors.As(runErr, &se) {
			report.Stage = se.stage
		}
		code = exitCode(class)
		l.log.Infof("%s %s: %v", scenario.Name(), strings.ToUpper(string(class)), runErr)
	} else {
		report.Result = string(result)
	}
	if err := report.Write(st.Dir()); err != nil {
		l.log.Infof("error: writing report: %v", err)
	}
	if st.Kept {
		l.printKept(st)
	}
	l.log.Infof("report: %s", relPath(c.RepoRoot, filepath.Join(st.Dir(), "report.json")))
	l.log.Infof("serial: %s", relPath(c.RepoRoot, filepath.Join(st.Dir(), "serial.log")))
	return code
}

func (l *Lab) execute(ctx context.Context, st *State, report *Report, opts RunOptions, scenario Scenario) (Result, error) {
	version, err := l.api.Version(ctx)
	if err != nil {
		return "", fail(ClassInfra, "api", err)
	}
	report.Proxmox.Version = version.Version
	if err := st.Advance(PhasePreflighted); err != nil {
		return "", fail(ClassInfra, "state", err)
	}

	artifacts, err := ManifestArtifacts(l.cfg.RepoRoot, opts.ISO, opts.Disk)
	if err != nil {
		return "", fail(ClassConfig, "artifacts", err)
	}
	report.Artifacts.ISOSHA256 = artifacts.ISO.SHA256
	report.Artifacts.DiskSHA256 = artifacts.Disk.SHA256
	writeRunFile(st.Dir(), "artifacts.json", artifacts)
	if err := st.Advance(PhaseArtifactsReady); err != nil {
		return "", fail(ClassInfra, "state", err)
	}

	if err := l.CreateVM(ctx, st); err != nil {
		return "", err
	}
	report.Proxmox.VMID = st.VMID
	l.log.Infof("vm %d cpu=%s sockets=%d cores=%d memory=%dMiB", st.VMID, l.cfg.CPU, l.cfg.Sockets, l.cfg.Cores, l.cfg.Memory)

	if err := l.UploadISO(ctx, st, artifacts.ISO, opts.ISO); err != nil {
		return "", err
	}
	l.log.Infof("iso uploaded")
	volume, err := l.ImportDisk(ctx, st, artifacts.Disk, opts.Disk)
	if err != nil {
		return "", err
	}
	l.log.Infof("disk imported")
	vmConfig, err := l.ConfigureVM(ctx, st, volume)
	if err != nil {
		return "", err
	}
	writeRunFile(st.Dir(), "vm-config.json", vmConfig)
	writeRunFile(st.Dir(), "host-info.json", l.HostInfo(ctx, version.Version))

	// The console connects before the VM starts and waits for its socket,
	// so the serial log has the whole boot.
	console, err := AttachSerial(l.ssh, st.VMID, filepath.Join(st.Dir(), "serial.log"))
	if err != nil {
		return "", fail(ClassInfra, "console", err)
	}
	defer console.Close()
	if err := l.StartVM(ctx, st); err != nil {
		return "", err
	}
	l.log.Infof("vm started")
	if err := st.Advance(PhaseConsoleAttached); err != nil {
		return "", fail(ClassInfra, "state", err)
	}

	guest := &Guest{con: console, logf: l.log.Debugf}
	defer func() { report.Checks = guest.checks }()
	if err := guest.WaitBoot(ctx, l.cfg.Timeout); err != nil {
		return "", err
	}
	l.log.Infof("boot mode normal")
	l.log.Infof("shell ready")

	st.Advance(PhaseScenarioRunning)
	session := &Session{
		Guest: guest, Report: report, RequirePass: opts.RequirePass, KeepRunning: l.cfg.Keep, WorkloadTimeout: opts.WorkloadTimeout,
		PowerState: func(ctx context.Context) (string, error) {
			s, err := l.api.VMStatus(ctx, st.VMID)
			return s.Status, err
		},
		logf: l.log.Debugf,
	}
	result, err := scenario.Run(ctx, session)
	if ctx.Err() != nil && err != nil {
		return "", fail(ClassInfra, "interrupted", ctx.Err())
	}
	if err != nil {
		return "", err
	}
	st.Advance(PhaseScenarioComplete)
	l.reportScenario(scenario, result, report)
	return result, nil
}

func (l *Lab) reportScenario(scenario Scenario, result Result, report *Report) {
	if w := report.Workload; w != nil && !w.Completed {
		l.log.Infof("%s workload did not complete: %s", scenario.Name(), w.Ending)
	}
	if d := report.Linux; d != nil {
		l.log.Infof("linux syscalls=%d unsupported=%d refused=%s recent=%s bad-pointers=%d",
			d.Syscalls, d.Unsupported, joinNumbers(d.Refused), joinNumbers(d.RecentUnsupported), d.BadPointers)
	}
	switch result {
	case ResultPass:
		l.log.Infof("%s PASS", scenario.Name())
	default:
		l.log.Infof("result %s", result)
	}
}

func joinNumbers(ns []uint64) string {
	if len(ns) == 0 {
		return "none"
	}
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.FormatUint(n, 10)
	}
	return strings.Join(parts, ",")
}

func (l *Lab) printKept(st *State) {
	l.log.Infof("VM retained for debugging")
	l.log.Infof("run-id: %s", st.RunID)
	l.log.Infof("node:   %s", st.Node)
	l.log.Infof("vmid:   %d", st.VMID)
	l.log.Infof("state:  %s", relPath(l.cfg.RepoRoot, filepath.Join(st.Dir(), "state.json")))
	sudo := ""
	if l.ssh.Sudo {
		sudo = "sudo "
	}
	l.log.Infof("serial: ssh -t -p %d %s@%s %sqm terminal %d", l.ssh.Port, l.ssh.User, l.ssh.Host, sudo, st.VMID)
	l.log.Infof("remove: go run ./tools/pvelab destroy --run %s", st.RunID)
}

func relPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// fileExists is used by doctor for the artifact checks.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
