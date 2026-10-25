package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/csmith/envflag/v2"
	"github.com/csmith/slogflags"
	"github.com/docker/docker/client"
)

const (
	labelPreBackupCmd  = "com.greboid.bailer.pre-backup-cmd"
	labelPostBackupCmd = "com.greboid.bailer.post-backup-cmd"
	labelExclude       = "com.greboid.bailer.exclude"
	labelInclude       = "com.greboid.bailer.include"

	labelHelperImage   = "com.greboid.bailer.helper-image"
	labelHelperPreCmd  = "com.greboid.bailer.helper-pre-cmd"
	labelHelperPostCmd = "com.greboid.bailer.helper-post-cmd"
)

var (
	backupDir  = flag.String("backup-dir", "/backups", "Directory to backups")
	mode       = flag.String("mode", "exclude", "Filtering mode: exclude or include")
	once       = flag.Bool("once", false, "Run a single backup and exit")
	dryRun     = flag.Bool("dry-run", false, "Show what would be backed up without performing any actions")
	daemon     = flag.Duration("daemon", 0, "Run as a daemon, performing backups intervals")
	backend    = flag.String("backend", "local", "Backup backend: local or restic")
	resticBin  = flag.String("restic-bin", "restic", "Path to the restic binary")
	resticHost = flag.String("restic-host", "", "Host identity used for restic snapshots (defaults to the machine hostname)")
)

func main() {
	// The export-volume subcommand is an internal entry point used by the
	// restic backend: it must run before global flag parsing.
	if len(os.Args) > 1 && os.Args[1] == "export-volume" {
		os.Exit(runExportCommand(os.Args[2:]))
	}

	envflag.Parse()
	_ = slogflags.Logger(slogflags.WithSetDefault(true))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *mode != "exclude" && *mode != "include" {
		slog.Error("Invalid mode, must be 'exclude' or 'include'", "mode", *mode)
		os.Exit(1)
	}

	if err := validateBackend(*backend, *resticBin, exec.LookPath); err != nil {
		slog.Error("Invalid backend configuration", "error", err)
		os.Exit(1)
	}

	rawCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error("Failed to create Docker client", "error", err)
		os.Exit(1)
	}
	defer rawCli.Close()

	cli := dockerClient{rawCli}

	backendImpl, err := newBackend(*backend, cli, *backupDir, *resticBin, *resticHost)
	if err != nil {
		slog.Error("Failed to create backup backend", "error", err)
		os.Exit(1)
	}

	if *dryRun {
		if err := dryRunMode(ctx, cli, *mode); err != nil {
			slog.Error("Dry run failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *daemon > 0 {
		runDaemon(ctx, cli, backendImpl, *mode, *daemon)
		return
	}

	if *once {
		if err := run(ctx, cli, backendImpl, *mode); err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	slog.Error("No action specified: use -once, -dry-run, or -daemon")
	os.Exit(1)
}

// newBackend resolves the selected backend. Repository configuration is read
// from the standard restic environment variables by the restic binary itself.
func newBackend(name string, cli dockerAPI, backupDirName, resticBinName, host string) (BackupBackend, error) {
	switch name {
	case "local":
		return &LocalTarBackend{cli: cli, backupDir: backupDirName}, nil
	case "restic":
		if host == "" {
			hostname, err := os.Hostname()
			if err != nil || hostname == "" {
				return nil, fmt.Errorf("determining host identity: %w", err)
			}
			host = hostname
		}
		selfPath, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolving bailer executable for export-volume: %w", err)
		}
		return &ResticBackend{cli: cli, resticBin: resticBinName, host: host, selfPath: selfPath}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q, must be 'local' or 'restic'", name)
	}
}
