package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/docker/docker/client"
)

// runExportCommand implements the internal `bailer export-volume` entry point
// used by the tar mode of the restic backend. It writes an uncompressed tar
// stream of the given container path to stdout and nothing else: all
// diagnostics go to stderr, so the stream is always pure tar data. It exits
// non-zero on any failure and never runs pre/post hooks or a backup pass.
func runExportCommand(args []string) int {
	fs := flag.NewFlagSet("export-volume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	containerID := fs.String("container-id", "", "ID of the container to export from")
	path := fs.String("path", "", "Absolute path inside the container to export")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *containerID == "" {
		fmt.Fprintln(os.Stderr, "bailer export-volume: --container-id is required")
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "bailer export-volume: --path is required")
		return 2
	}
	if err := validateExportPath(*path); err != nil {
		fmt.Fprintf(os.Stderr, "bailer export-volume: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Die with the restic process that spawned us so a cancelled backup can
	// never leave the exporter running (Linux PR_SET_PDEATHSIG).
	if err := setupParentDeathSignal(); err != nil {
		slog.Error("Failed to set parent death signal", "error", err)
		return 1
	}

	rawCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error("Failed to create Docker client", "error", err)
		return 1
	}
	defer rawCli.Close()

	if err := exportVolume(ctx, dockerClient{rawCli}, *containerID, *path, os.Stdout); err != nil {
		slog.Error("Export failed", "container", *containerID, "path", *path, "error", err)
		return 1
	}
	return 0
}

// exportVolume streams the tar archive of containerID:path produced by
// Docker's CopyFromContainer into dst. The archive entries are rooted at the
// base name of path (exporting /data yields a top-level "data" entry), which
// must be preserved for restoration semantics.
func exportVolume(ctx context.Context, cli dockerAPI, containerID, path string, dst io.Writer) error {
	if containerID == "" {
		return errors.New("container id must not be empty")
	}
	if err := validateExportPath(path); err != nil {
		return err
	}

	reader, err := cli.copyFromContainer(ctx, containerID, path)
	if err != nil {
		return fmt.Errorf("copying from container: %w", err)
	}

	_, copyErr := io.Copy(dst, reader)
	closeErr := reader.Close()
	if copyErr != nil {
		return fmt.Errorf("streaming tar archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing docker response stream: %w", closeErr)
	}
	return nil
}

// validateExportPath rejects paths whose meaning is unclear or that would
// export more than a single mount: only canonical absolute paths are
// accepted, and the container root filesystem is refused outright.
func validateExportPath(path string) error {
	if path == "" {
		return errors.New("path must not be empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("path %q must be absolute", path)
	}
	clean := filepath.Clean(path)
	if clean != path {
		return fmt.Errorf("path %q is not canonical, use %q", path, clean)
	}
	if clean == "/" {
		return errors.New("refusing to export the container root filesystem")
	}
	return nil
}
