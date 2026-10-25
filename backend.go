package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// BackupRequest describes a single mount of a container to back up. The
// concrete container and mount chosen by the discovery/filtering pass are
// passed through unchanged so backends never re-discover containers.
type BackupRequest struct {
	ContainerID   string
	ContainerName string
	Mount         container.MountPoint
}

// BackupBackend stores the exported contents of one mount.
type BackupBackend interface {
	Backup(ctx context.Context, request BackupRequest) error
}

// LocalTarBackend is the original behaviour: a gzip-compressed tar archive
// with a timestamped name under backupDir/<container>/.
type LocalTarBackend struct {
	cli       dockerAPI
	backupDir string
}

func (b *LocalTarBackend) Backup(ctx context.Context, request BackupRequest) error {
	timestamp := time.Now().Format("20060102-150405")
	id := mountIdentifier(request.Mount)
	sanitized := strings.ReplaceAll(strings.Trim(id, "/"), "/", "_")
	backupName := fmt.Sprintf("%s_%s.tar.gz", sanitized, timestamp)

	containerDir := filepath.Join(b.backupDir, request.ContainerName)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("creating backup directory: %w", err)
	}
	backupPath := filepath.Join(containerDir, backupName)

	return writeLocalArchive(ctx, b.cli, request.ContainerID, request.Mount.Destination, backupPath)
}

// writeLocalArchive streams the mount into a temporary file and atomically
// renames it into place only after the archive is fully finalised, so a
// failed backup never leaves a plausible-looking archive behind.
func writeLocalArchive(ctx context.Context, cli dockerAPI, containerID, path, backupPath string) error {
	reader, err := cli.copyFromContainer(ctx, containerID, path)
	if err != nil {
		return fmt.Errorf("copying from container: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(backupPath), "."+filepath.Base(backupPath)+".tmp-")
	if err != nil {
		_ = reader.Close()
		return fmt.Errorf("creating temporary backup file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	gzWriter := gzip.NewWriter(tmpFile)
	_, copyErr := io.Copy(gzWriter, reader)
	closeErr := reader.Close()
	gzErr := gzWriter.Close()
	fileErr := tmpFile.Close()

	if copyErr != nil {
		return fmt.Errorf("writing backup file: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing docker response stream: %w", closeErr)
	}
	if gzErr != nil {
		return fmt.Errorf("finalising archive: %w", gzErr)
	}
	if fileErr != nil {
		return fmt.Errorf("closing backup file: %w", fileErr)
	}

	if err := os.Rename(tmpPath, backupPath); err != nil {
		return fmt.Errorf("publishing backup file: %w", err)
	}
	return nil
}

// ResticBackend streams each mount as a raw tar archive from the
// export-volume subcommand of this same binary directly into a restic
// snapshot, delegating deduplication, encryption and remote transport to the
// restic binary. Repository configuration (RESTIC_REPOSITORY,
// RESTIC_PASSWORD_FILE, ...) is inherited from the environment.
type ResticBackend struct {
	cli       dockerAPI
	resticBin string
	host      string
	selfPath  string
}

func (b *ResticBackend) Backup(ctx context.Context, request BackupRequest) error {
	cmd := exec.CommandContext(ctx, b.resticBin, resticBackupArgs(b.selfPath, b.host, request)...)
	// Restic prints progress to stderr and its summary to stdout. Neither
	// must ever reach bailer's stdout: the export-volume subprocess writes
	// the tar stream there. Output is captured and forwarded to slog at
	// debug level; restic inherits the environment so RESTIC_* variables
	// apply.
	out := &slogWriter{logger: slog.Default(), level: slog.LevelDebug}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		out.Flush()
		// The error may be a context cancellation or a restic failure; the
		// full diagnostics went to slog at debug level and the tail is
		// included here so failures stay diagnosable at any log level.
		return fmt.Errorf("restic backup of %s: %w: %s", stableArchiveName(request), err, out.tail())
	}
	return nil
}

// slogWriter forwards subprocess output to slog line by line at the given
// level, collapsing carriage-return progress rewrites into separate lines.
// It retains everything written so failures can include a tail in errors.
type slogWriter struct {
	logger *slog.Logger
	level  slog.Level

	pending bytes.Buffer // partial line awaiting its newline
	all     bytes.Buffer // full output for error reporting
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.all.Write(p)
	w.pending.Write(p)
	for {
		line, err := w.pending.ReadString('\n')
		if err != nil {
			w.pending.Reset()
			w.pending.WriteString(line)
			break
		}
		w.emit(line)
	}
	return len(p), nil
}

// Flush emits a trailing partial line; call once the subprocess exits.
func (w *slogWriter) Flush() {
	if w.pending.Len() > 0 {
		w.emit(w.pending.String())
		w.pending.Reset()
	}
}

func (w *slogWriter) emit(line string) {
	for part := range strings.SplitSeq(strings.TrimRight(line, "\n"), "\r") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		w.logger.Log(context.Background(), w.level, part)
	}
}

func (w *slogWriter) tail() string {
	return outputSummary(w.all.String())
}

// resticBackupArgs builds the restic argv for one mount. The tar stream is
// produced by running this binary's export-volume subcommand via restic's
// --stdin-from-command, so no intermediate archive file exists.
func resticBackupArgs(selfPath, host string, request BackupRequest) []string {
	return []string{
		"backup",
		"--host", host,
		"--tag", "bailer",
		"--tag", "container:" + request.ContainerName,
		"--tag", "volume:" + mountIdentifier(request.Mount),
		"--tag", "path:" + request.Mount.Destination,
		"--stdin-filename", stableArchiveName(request),
		"--stdin-from-command",
		"--", selfPath, "export-volume",
		"--container-id", request.ContainerID,
		"--path", request.Mount.Destination,
	}
}

// stableArchiveName returns a deterministic, filesystem-safe filename for a
// mount. It intentionally excludes timestamps: the identity of a mount must
// stay consistent across backups so snapshot names remain comparable. Within
// a single host, container name plus mount destination is unique (a container
// cannot mount two volumes at the same destination); for named volumes the
// volume name is also included so the same volume mounted twice at different
// destinations stays distinguishable.
func stableArchiveName(request BackupRequest) string {
	parts := []string{request.ContainerName, mountIdentifier(request.Mount)}
	if request.Mount.Type == mount.TypeVolume {
		parts = append(parts, request.Mount.Destination)
	}
	return sanitizeFilename(strings.Join(parts, "_")) + ".tar"
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// resticEnv holds the standard restic configuration variables bailer relies
// on being present. Values are never logged.
type resticEnv struct {
	repository      string
	repositoryFile  string
	password        string
	passwordFile    string
	passwordCommand string
}

func resticEnvFromOS() resticEnv {
	return resticEnv{
		repository:      os.Getenv("RESTIC_REPOSITORY"),
		repositoryFile:  os.Getenv("RESTIC_REPOSITORY_FILE"),
		password:        os.Getenv("RESTIC_PASSWORD"),
		passwordFile:    os.Getenv("RESTIC_PASSWORD_FILE"),
		passwordCommand: os.Getenv("RESTIC_PASSWORD_COMMAND"),
	}
}

func (e resticEnv) validate() error {
	if e.repository == "" && e.repositoryFile == "" {
		return errors.New("restic repository not configured: set RESTIC_REPOSITORY or RESTIC_REPOSITORY_FILE")
	}
	if e.password == "" && e.passwordFile == "" && e.passwordCommand == "" {
		return errors.New("restic password not configured: set RESTIC_PASSWORD_FILE, RESTIC_PASSWORD or RESTIC_PASSWORD_COMMAND")
	}
	return nil
}

// validateBackend checks the backend selection and, for restic, that the
// binary and repository configuration exist before a backup pass starts. The
// repository itself is assumed to have been created with `restic init`.
func validateBackend(backend, resticBin string, lookup func(string) (string, error)) error {
	switch backend {
	case "local":
		return nil
	case "restic":
		if _, err := lookup(resticBin); err != nil {
			return fmt.Errorf("restic binary %q not found: %w", resticBin, err)
		}
		return resticEnvFromOS().validate()
	default:
		return fmt.Errorf("unknown backend %q, must be 'local' or 'restic'", backend)
	}
}
