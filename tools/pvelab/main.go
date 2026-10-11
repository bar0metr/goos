// Command pvelab runs disposable GoOS VMs on a Proxmox VE node: it uploads
// the boot ISO and a fresh installed disk, boots them under KVM, drives the
// serial console through a scenario, writes a report, and removes every
// resource it made. It is development infrastructure, no part of GoOS, and
// no part of make release-check. See docs/development/proxmox-lab.md.
//
//	pvelab doctor
//	pvelab run --scenario smoke|linux|7zip [--keep] [--cores N] ...
//	pvelab resume --run RUN_ID
//	pvelab destroy --run RUN_ID
//	pvelab cleanup --stale 6h [--yes]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const usage = `usage: pvelab <command> [flags]

commands:
  doctor    check configuration, credentials, privileges, storage, and SSH
  run       boot a disposable VM and run a scenario
  resume    rerun the saved scenario on a kept VM after verifying its source
  destroy   remove the resources of a run from its saved state
  cleanup   remove the resources of uncleaned runs older than --stale

Run "pvelab <command> -h" for the flags of a command.
`

func main() {
	os.Exit(mainCode(os.Args[1:], os.Stdout, os.Stderr))
}

// common holds the flags every command takes.
type common struct {
	envFile string
	verbose bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.envFile, "env-file", "", "dotenv file (default <repository>/.env)")
	fs.BoolVar(&c.verbose, "verbose", false, "log API calls, tasks, remote commands, and checks")
}

func mainCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return ExitConfig
		}
		return ExitPass
	}
	command, args := args[0], args[1:]
	fs := flag.NewFlagSet("pvelab "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var com common
	com.register(fs)

	var (
		scenario, iso, disk, cpu, runID, sourceEvidenceSHA256 string
		sockets, cores, memory                                int
		timeout, workloadTimeout, stale                       time.Duration
		keep, requirePass, requireClean                       bool
		yes                                                   bool
	)
	switch command {
	case "doctor":
		fs.StringVar(&iso, "iso", "", "also check that this ISO exists")
		fs.StringVar(&disk, "disk", "", "also check that this disk image exists")
	case "run":
		fs.StringVar(&scenario, "scenario", "smoke", "scenario: "+scenarioNames())
		fs.StringVar(&iso, "iso", "build/pve/input/goos.iso", "boot ISO")
		fs.StringVar(&disk, "disk", "build/pve/input/goos-disk.img", "fresh installed-system disk")
		fs.StringVar(&cpu, "cpu", "", "Proxmox CPU model (PVE_CPU)")
		fs.IntVar(&sockets, "sockets", 0, "CPU sockets (PVE_SOCKETS)")
		fs.IntVar(&cores, "cores", 0, "cores per socket (PVE_CORES)")
		fs.IntVar(&memory, "memory", 0, "memory in MiB (PVE_MEMORY)")
		fs.DurationVar(&timeout, "timeout", 0, "time from VM start to the shell prompt (PVE_TIMEOUT)")
		fs.DurationVar(&workloadTimeout, "workload-timeout", 60*time.Second, "time a diagnostic workload may run")
		fs.BoolVar(&keep, "keep", false, "keep the VM and its ISO for debugging (PVE_KEEP_VM)")
		fs.BoolVar(&requirePass, "require-pass", false, "fail a diagnostic workload that does not complete")
		fs.BoolVar(&requireClean, "require-clean", false, "refuse a working tree with uncommitted changes")
	case "resume":
		fs.StringVar(&runID, "run", "", "kept source run ID to resume")
		fs.StringVar(&sourceEvidenceSHA256, "source-evidence-sha256", "", "trusted SHA-256 of retained source evidence")
		fs.BoolVar(&requireClean, "require-clean", false, "refuse a working tree with uncommitted changes")
	case "destroy":
		fs.StringVar(&runID, "run", "", "run ID to destroy")
	case "cleanup":
		fs.DurationVar(&stale, "stale", 0, "clean runs not updated for this long, e.g. 6h")
		fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	default:
		fmt.Fprintf(stderr, "pvelab: unknown command %q\n\n%s", command, usage)
		return ExitConfig
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return ExitPass
		}
		return ExitConfig
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pvelab %s: unexpected argument %q\n", command, fs.Arg(0))
		return ExitConfig
	}
	log := &Logger{w: stdout, verbose: com.verbose}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}
	root, err := FindRepoRoot(wd)
	if err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}
	envFile := com.envFile
	if envFile != "" {
		envFile, _ = filepath.Abs(envFile)
	}
	cfg, err := LoadConfig(root, envFile, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}
	// Explicit flags override the environment and .env.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cpu":
			cfg.CPU = cpu
		case "sockets":
			cfg.Sockets = sockets
		case "cores":
			cfg.Cores = cores
		case "memory":
			cfg.Memory = memory
		case "timeout":
			cfg.Timeout = timeout
		case "keep":
			cfg.Keep = keep
		}
	})
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	iso, disk = abs(iso), abs(disk)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if command == "doctor" {
		return Doctor(ctx, cfg, log, []string{iso, disk})
	}
	if err := CheckEnvIgnored(root, cfg.EnvFile); err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}
	lab, err := NewLab(cfg, log)
	if err != nil {
		fmt.Fprintf(stderr, "pvelab: %v\n", err)
		return ExitConfig
	}

	switch command {
	case "run":
		if _, ok := scenarios[scenario]; !ok {
			fmt.Fprintf(stderr, "pvelab run: unknown scenario %q (want %s)\n", scenario, scenarioNames())
			return ExitConfig
		}
		if workloadTimeout <= 0 {
			fmt.Fprintln(stderr, "pvelab run: --workload-timeout must be positive")
			return ExitConfig
		}
		return lab.Run(ctx, RunOptions{
			Scenario: scenario, ISO: iso, Disk: disk,
			RequirePass: requirePass, RequireClean: requireClean, WorkloadTimeout: workloadTimeout,
		})
	case "resume":
		if runID == "" {
			fmt.Fprintln(stderr, "pvelab resume: --run RUN_ID is required")
			return ExitConfig
		}
		if !requireClean {
			fmt.Fprintln(stderr, "pvelab resume: --require-clean is required")
			return ExitConfig
		}
		if !validSHA256(sourceEvidenceSHA256) {
			fmt.Fprintln(stderr, "pvelab resume: --source-evidence-sha256 SHA256 is required")
			return ExitConfig
		}
		return lab.Resume(ctx, runID, sourceEvidenceSHA256)
	case "destroy":
		if runID == "" {
			fmt.Fprintln(stderr, "pvelab destroy: --run RUN_ID is required")
			return ExitConfig
		}
		return lab.Destroy(ctx, runID)
	case "cleanup":
		if stale <= 0 {
			fmt.Fprintln(stderr, "pvelab cleanup: --stale DURATION is required")
			return ExitConfig
		}
		return lab.CleanupStale(ctx, stale, yes)
	}
	return ExitConfig
}
