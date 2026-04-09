package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/csmith/envflag"
	"github.com/csmith/slogflags"
	"github.com/docker/docker/client"
)

const (
	labelPreBackupCmd  = "com.greboid.bailer.pre-backup-cmd"
	labelPostBackupCmd = "com.greboid.bailer.post-backup-cmd"
	labelExclude       = "com.greboid.bailer.exclude"
	labelInclude       = "com.greboid.bailer.include"
)

var (
	backupDir = flag.String("backup-dir", "/backups", "Directory to backups")
	mode      = flag.String("mode", "exclude", "Filtering mode: exclude or include")
	once      = flag.Bool("once", false, "Run a single backup and exit")
	dryRun    = flag.Bool("dry-run", false, "Show what would be backed up without performing any actions")
	daemon    = flag.Duration("daemon", 0, "Run as a daemon, performing backups intervals")
)

func main() {
	envflag.Parse()
	_ = slogflags.Logger(slogflags.WithSetDefault(true))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *mode != "exclude" && *mode != "include" {
		slog.Error("Invalid mode, must be 'exclude' or 'include'", "mode", *mode)
		os.Exit(1)
	}

	rawCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error("Failed to create Docker client", "error", err)
		os.Exit(1)
	}
	defer rawCli.Close()

	cli := dockerClient{rawCli}

	if *dryRun {
		if err := dryRunMode(ctx, cli, *mode); err != nil {
			slog.Error("Dry run failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *daemon > 0 {
		runDaemon(ctx, cli, *backupDir, *mode, *daemon)
		return
	}

	if *once {
		if err := run(ctx, cli, *backupDir, *mode); err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	slog.Error("No action specified: use -once, -dry-run, or -daemon")
	os.Exit(1)
}
