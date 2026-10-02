package main

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// requiredPrivileges is what the API token needs, by ACL path. VM.Console
// is absent: the serial console goes through SSH.
func requiredPrivileges(c *Config) map[string][]string {
	need := map[string][]string{
		"/pool/" + c.Pool: {
			"Pool.Audit", "VM.Allocate", "VM.Audit", "VM.PowerMgmt",
			"VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk", "VM.Config.CDROM",
			"VM.Config.HWType", "VM.Config.Options",
		},
		"/nodes/" + c.Node: {"Sys.Audit"},
	}
	add := func(path string, privs ...string) {
		for _, p := range privs {
			if !contains(need[path], p) {
				need[path] = append(need[path], p)
			}
		}
	}
	add("/storage/"+c.DiskStorage, "Datastore.AllocateSpace", "Datastore.Audit")
	add("/storage/"+c.EFIStorage, "Datastore.AllocateSpace", "Datastore.Audit")
	// Uploading an ISO needs AllocateTemplate; deleting one needs Allocate.
	add("/storage/"+c.ISOStorage, "Datastore.AllocateTemplate", "Datastore.Allocate", "Datastore.Audit")
	return need
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type doctorRow struct {
	name, status, detail string
}

type doctor struct {
	rows   []doctorRow
	failed bool
	class  Class
}

func (d *doctor) ok(name, detail string)   { d.rows = append(d.rows, doctorRow{name, "ok", detail}) }
func (d *doctor) warn(name, detail string) { d.rows = append(d.rows, doctorRow{name, "warn", detail}) }
func (d *doctor) bad(class Class, name, detail string) {
	d.rows = append(d.rows, doctorRow{name, "FAIL", detail})
	if !d.failed || class == ClassConfig {
		d.class = class
	}
	d.failed = true
}

// Doctor checks the configuration, the API, the token's privileges, the
// storages, SSH, and the node's tools. It creates nothing.
func Doctor(ctx context.Context, cfg *Config, log *Logger, artifacts []string) int {
	d := &doctor{}
	d.ok("repository", cfg.RepoRoot)
	if err := CheckEnvIgnored(cfg.RepoRoot, cfg.EnvFile); err != nil {
		d.bad(ClassConfig, ".env", err.Error())
	} else {
		d.ok(".env", "ignored by git")
	}
	if err := cfg.Validate(); err != nil {
		d.bad(ClassConfig, "config", err.Error())
		return d.print(log)
	}
	d.ok("config", "complete")
	for _, path := range artifacts {
		if path == "" {
			continue
		}
		if fileExists(path) {
			d.ok("artifact", relPath(cfg.RepoRoot, path))
		} else {
			d.bad(ClassConfig, "artifact", relPath(cfg.RepoRoot, path)+" missing; run make pve-artifacts")
		}
	}

	lab, err := NewLab(cfg, log)
	if err != nil {
		d.bad(ClassConfig, "tls", err.Error())
		return d.print(log)
	}
	if cfg.TLSInsecure {
		d.warn("tls", "certificate verification off (PVE_TLS_INSECURE=1)")
	} else if cfg.CAFile != "" {
		d.ok("tls", "verified against "+cfg.CAFile)
	} else {
		d.ok("tls", "verified against the system roots")
	}
	d.checkAPI(ctx, lab)
	d.checkSSH(ctx, lab)
	if cfg.CPU == "host" {
		d.ok("cpu", "host")
	} else {
		d.ok("cpu", cfg.CPU+" (checked when the VM is created)")
	}
	return d.print(log)
}

func (d *doctor) checkAPI(ctx context.Context, lab *Lab) {
	cfg, api := lab.cfg, lab.api
	version, err := api.Version(ctx)
	if err != nil {
		d.bad(ClassInfra, "api", err.Error())
		return
	}
	d.ok("api", "PVE "+version.Version)
	if !strings.HasPrefix(version.Version, "7.") && !strings.HasPrefix(version.Version, "8.") {
		d.warn("api version", "pvelab is tested on PVE 7.4; "+version.Version+" is untested")
	}

	nodes, err := api.Nodes(ctx)
	if err != nil {
		d.bad(ClassInfra, "node", err.Error())
	} else {
		state := ""
		for _, n := range nodes {
			if n.Node == cfg.Node {
				state = n.Status
			}
		}
		switch state {
		case "online":
			d.ok("node", cfg.Node)
		case "":
			d.bad(ClassConfig, "node", cfg.Node+" is not a node of this cluster")
		default:
			d.bad(ClassInfra, "node", cfg.Node+" is "+state)
		}
	}

	need := requiredPrivileges(cfg)
	paths := make([]string, 0, len(need))
	for p := range need {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	privilegesOK := true
	for _, path := range paths {
		have, err := api.Permissions(ctx, path)
		if err != nil {
			d.bad(ClassInfra, "privileges", err.Error())
			privilegesOK = false
			break
		}
		var missing []string
		for _, p := range need[path] {
			if !have[p] {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			d.bad(ClassConfig, "privileges", path+" lacks "+strings.Join(missing, ", "))
			privilegesOK = false
		}
	}
	if privilegesOK {
		d.ok("privileges", "token holds every privilege a run needs")
	}

	if err := api.Pool(ctx, cfg.Pool); err != nil {
		d.bad(ClassConfig, "pool", cfg.Pool+": "+err.Error())
	} else {
		d.ok("pool", cfg.Pool)
	}
	d.checkStorage(ctx, api, "disk storage", cfg.DiskStorage, "images")
	d.checkStorage(ctx, api, "iso storage", cfg.ISOStorage, "iso")
	d.checkStorage(ctx, api, "efi storage", cfg.EFIStorage, "images")
}

func (d *doctor) checkStorage(ctx context.Context, api *API, row, storage, content string) {
	s, err := api.StorageStatus(ctx, storage)
	if err != nil {
		d.bad(ClassConfig, row, storage+": "+err.Error())
		return
	}
	if s.Active == 0 {
		d.bad(ClassInfra, row, storage+" is not active on the node")
		return
	}
	if !contains(strings.Split(s.Content, ","), content) {
		d.bad(ClassConfig, row, fmt.Sprintf("%s does not hold %s content (it holds %s)", storage, content, s.Content))
		return
	}
	d.ok(row, storage+" ("+s.Type+")")
}

func (d *doctor) checkSSH(ctx context.Context, lab *Lab) {
	if _, err := exec.LookPath("ssh"); err != nil {
		d.bad(ClassConfig, "ssh", "no ssh binary on PATH")
		return
	}
	s := lab.ssh
	if _, err := s.Run(ctx, "true"); err != nil {
		d.bad(ClassInfra, "ssh", fmt.Sprintf("%s@%s:%d: %v", s.User, s.Host, s.Port, err))
		return
	}
	d.ok("ssh", fmt.Sprintf("%s@%s:%d", s.User, s.Host, s.Port))
	if s.Sudo {
		if _, err := s.Run(ctx, "sudo", "-n", "true"); err != nil {
			d.bad(ClassConfig, "sudo", "sudo -n refused; qm and the serial socket need root: "+err.Error())
			return
		}
		d.ok("sudo", "sudo -n works")
	}
	for _, tool := range []struct{ name, path string }{
		{"qm", "/usr/sbin/qm"}, {"pvesm", "/usr/sbin/pvesm"}, {"socat", "socat"}, {"gzip", "gzip"}, {"sha256sum", "sha256sum"},
	} {
		check := "command -v " + shellQuote(tool.path)
		if strings.HasPrefix(tool.path, "/") {
			check = "test -x " + shellQuote(tool.path)
		}
		if _, err := s.Run(ctx, "sh", "-c", check); err != nil {
			d.bad(ClassInfra, tool.name, "missing on the node")
		} else {
			d.ok(tool.name, "")
		}
	}
	arch, err := s.Run(ctx, "uname", "-m")
	if a := strings.TrimSpace(string(arch)); err != nil || a != "x86_64" {
		d.bad(ClassInfra, "arch", fmt.Sprintf("node is %q, want x86_64", a))
	} else {
		d.ok("arch", a)
	}
	if _, err := s.Run(ctx, "test", "-c", "/dev/kvm"); err != nil {
		d.bad(ClassInfra, "kvm", "/dev/kvm missing on the node")
	} else {
		d.ok("kvm", "/dev/kvm")
	}
}

func (d *doctor) print(log *Logger) int {
	fmt.Fprintln(log.w, "pvelab doctor")
	for _, r := range d.rows {
		line := fmt.Sprintf("%-16s %-4s", r.name, r.status)
		if r.detail != "" {
			line += " " + r.detail
		}
		fmt.Fprintln(log.w, strings.TrimRight(line, " "))
	}
	if d.failed {
		fmt.Fprintf(log.w, "%-16s %s\n", "result", "FAIL")
		return exitCode(d.class)
	}
	fmt.Fprintf(log.w, "%-16s %s\n", "result", "PASS")
	return ExitPass
}
