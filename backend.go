package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// Snapshot layout modes for the restic backend.
const (
	// resticModeTar streams each mount as a single tar archive into restic
	// via stdin. resticModeFiles stages the extracted volume contents on
	// disk so restic snapshots the individual files.
	resticModeTar   = "tar"
	resticModeFiles = "files"
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

// ResticBackend stores each mount in a restic snapshot, delegating
// deduplication, encryption and remote transport to the restic binary. In tar
// mode a raw tar archive from the export-volume subcommand of this same
// binary is streamed straight into restic; in files mode the volume contents
// are staged on disk and snapshotted as individual files. Repository
// configuration (RESTIC_REPOSITORY, RESTIC_PASSWORD_FILE, ...) is inherited
// from the environment.
type ResticBackend struct {
	cli        dockerAPI
	resticBin  string
	host       string
	selfPath   string
	mode       string
	stagingDir string
}

func (b *ResticBackend) Backup(ctx context.Context, request BackupRequest) error {
	if b.mode == resticModeFiles {
		return b.backupFiles(ctx, request)
	}
	return b.backupTar(ctx, request)
}

// backupTar streams the tar archive produced by the export-volume
// subcommand straight into restic via --stdin-from-command, so no
// intermediate archive file exists.
func (b *ResticBackend) backupTar(ctx context.Context, request BackupRequest) error {
	return b.runRestic(ctx, resticBackupArgs(b.selfPath, b.host, request), stableArchiveName(request))
}

// backupFiles stages the volume contents in a local directory and lets
// restic snapshot the individual files, which deduplicates per file and
// allows native restic browsing and restore. Restic cannot ingest a
// multi-file stream, so staging is unavoidable. The staging path is
// deterministic per mount (stable across runs) so restic finds the previous
// snapshot as its incremental parent.
func (b *ResticBackend) backupFiles(ctx context.Context, request BackupRequest) error {
	if err := validateExportPath(request.Mount.Destination); err != nil {
		return err
	}

	reader, err := b.cli.copyFromContainer(ctx, request.ContainerID, request.Mount.Destination)
	if err != nil {
		return fmt.Errorf("copying from container: %w", err)
	}

	stagingRoot := filepath.Join(b.stagingDir, mountIdentity(request))
	// The archive entries are rooted at the base name of the mount
	// destination (exporting /data yields a top-level "data" entry), so the
	// snapshot shows the same layout as the tar archives.
	stagingPath := filepath.Join(stagingRoot, filepath.Base(request.Mount.Destination))
	defer os.RemoveAll(stagingRoot)

	// A leftover staging tree from a crashed run must go, and read-only
	// directories from a previous pass would block its removal.
	if err := clearStaging(stagingRoot); err != nil {
		_ = reader.Close()
		return fmt.Errorf("clearing staging directory: %w", err)
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		_ = reader.Close()
		return fmt.Errorf("creating staging directory: %w", err)
	}

	extractErr := extractTarStream(reader, stagingRoot)
	closeErr := reader.Close()
	if extractErr != nil {
		return fmt.Errorf("staging %s: %w", mountIdentity(request), extractErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing docker response stream: %w", closeErr)
	}

	return b.runRestic(ctx, resticFileBackupArgs(b.host, stagingPath, request), mountIdentity(request))
}

// runRestic executes the restic binary with the given arguments. Restic
// prints progress to stderr and its summary to stdout; neither must ever
// reach bailer's stdout. Output is captured and forwarded to slog at debug
// level; restic inherits the environment so RESTIC_* variables apply.
func (b *ResticBackend) runRestic(ctx context.Context, args []string, label string) error {
	cmd := exec.CommandContext(ctx, b.resticBin, args...)
	out := &slogWriter{logger: slog.Default(), level: slog.LevelDebug}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		out.Flush()
		// The error may be a context cancellation or a restic failure; the
		// full diagnostics went to slog at debug level and the tail is
		// included here so failures stay diagnosable at any log level.
		return fmt.Errorf("restic backup of %s: %w: %s", label, err, out.tail())
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

// resticTagArgs builds the tag arguments shared by both snapshot layouts so
// filtering and retention behave identically regardless of mode.
func resticTagArgs(request BackupRequest) []string {
	return []string{
		"--tag", "bailer",
		"--tag", "container:" + request.ContainerName,
		"--tag", "volume:" + mountIdentifier(request.Mount),
		"--tag", "path:" + request.Mount.Destination,
	}
}

// resticBackupArgs builds the restic argv for one mount in tar mode. The tar
// stream is produced by running this binary's export-volume subcommand via
// restic's --stdin-from-command, so no intermediate archive file exists.
func resticBackupArgs(selfPath, host string, request BackupRequest) []string {
	args := append([]string{"backup", "--host", host}, resticTagArgs(request)...)
	return append(args,
		"--stdin-filename", stableArchiveName(request),
		"--stdin-from-command",
		"--", selfPath, "export-volume",
		"--container-id", request.ContainerID,
		"--path", request.Mount.Destination,
	)
}

// resticFileBackupArgs builds the restic argv for one mount in files mode,
// snapshotting the staged directory tree as real files.
func resticFileBackupArgs(host, stagingPath string, request BackupRequest) []string {
	args := append([]string{"backup", "--host", host}, resticTagArgs(request)...)
	return append(args, stagingPath)
}

// stableArchiveName returns a deterministic, filesystem-safe filename for a
// mount. It intentionally excludes timestamps: the identity of a mount must
// stay consistent across backups so snapshot names remain comparable. Within
// a single host, container name plus mount destination is unique (a container
// cannot mount two volumes at the same destination); for named volumes the
// volume name is also included so the same volume mounted twice at different
// destinations stays distinguishable.
func stableArchiveName(request BackupRequest) string {
	return mountIdentity(request) + ".tar"
}

// mountIdentity is the stable, filesystem-safe identity of a mount without
// the tar suffix. It is also used as the staging directory name in files
// mode.
func mountIdentity(request BackupRequest) string {
	parts := []string{request.ContainerName, mountIdentifier(request.Mount)}
	if request.Mount.Type == mount.TypeVolume {
		parts = append(parts, request.Mount.Destination)
	}
	return sanitizeFilename(strings.Join(parts, "_"))
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
// binary, repository configuration and mode/staging settings exist before a
// backup pass starts. The staging directory for files mode is the backup
// directory. The repository itself is assumed to have been created with
// `restic init`.
func validateBackend(backend, resticBin, resticMode, backupDir string, lookup func(string) (string, error)) error {
	switch backend {
	case "local":
		return nil
	case "restic":
		if _, err := lookup(resticBin); err != nil {
			return fmt.Errorf("restic binary %q not found: %w", resticBin, err)
		}
		if err := resticEnvFromOS().validate(); err != nil {
			return err
		}
		if resticMode != resticModeTar && resticMode != resticModeFiles {
			return fmt.Errorf("unknown restic mode %q, must be 'tar' or 'files'", resticMode)
		}
		if resticMode == resticModeFiles && filepath.Clean(backupDir) == "/" {
			return errors.New("backup directory must not be the filesystem root in restic files mode")
		}
		return nil
	default:
		return fmt.Errorf("unknown backend %q, must be 'local' or 'restic'", backend)
	}
}

// clearStaging removes a previous staging tree. Read-only directories from
// an earlier extraction would otherwise be impossible to delete.
func clearStaging(root string) error {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// extractTarStream unpacks a tar stream (as produced by Docker's
// CopyFromContainer) below dst. Entries are written exactly as named, so the
// archive root — the base name of the exported path — is preserved.
// Permissions, modification times and, when bailer runs with sufficient
// privileges, ownership are restored. Entries that would escape dst are
// refused; special file types that cannot be staged are skipped.
func extractTarStream(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	var dirs []*stagedEntry
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar archive: %w", err)
		}
		dir, err := stageTarEntry(tr, header, dst)
		if err != nil {
			return err
		}
		if dir != nil {
			dirs = append(dirs, dir)
		}
	}
	// Directory metadata is applied deepest-first once all children exist:
	// writing children would clobber the mtime, and restrictive modes (e.g.
	// a read-only directory) would block the children themselves.
	for _, dir := range slices.Backward(dirs) {
		if err := applyMetadata(dir.path, dir.header); err != nil {
			return fmt.Errorf("restoring metadata of %s: %w", dir.header.Name, err)
		}
	}
	return nil
}

type stagedEntry struct {
	path   string
	header *tar.Header
}

func stageTarEntry(tr *tar.Reader, header *tar.Header, dst string) (*stagedEntry, error) {
	cleaned := path.Clean(header.Name)
	if path.IsAbs(header.Name) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return nil, fmt.Errorf("refusing unsafe tar entry %q", header.Name)
	}
	if cleaned == "." {
		return nil, nil
	}
	target := filepath.Join(dst, filepath.FromSlash(cleaned))

	switch header.Typeflag {
	case tar.TypeDir:
		// Created permissive; the archive's real mode is applied after the
		// children have been written.
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, fmt.Errorf("creating directory %s: %w", header.Name, err)
		}
		return &stagedEntry{path: target, header: header}, nil
	case tar.TypeReg:
		if err := stageParentDir(target); err != nil {
			return nil, err
		}
		if err := writeTarFile(tr, target, header.FileInfo().Mode()); err != nil {
			return nil, fmt.Errorf("writing file %s: %w", header.Name, err)
		}
		if err := applyMetadata(target, header); err != nil {
			return nil, fmt.Errorf("restoring metadata of %s: %w", header.Name, err)
		}
		return nil, nil
	case tar.TypeSymlink:
		// The link target is stored verbatim, even when it points outside
		// the staging tree: restic archives symlinks without following them.
		if err := stageParentDir(target); err != nil {
			return nil, err
		}
		if err := os.Symlink(header.Linkname, target); err != nil {
			return nil, fmt.Errorf("creating symlink %s: %w", header.Name, err)
		}
		return nil, nil
	case tar.TypeLink:
		linkCleaned := path.Clean(header.Linkname)
		if path.IsAbs(header.Linkname) || linkCleaned == ".." || strings.HasPrefix(linkCleaned, "../") {
			return nil, fmt.Errorf("refusing unsafe hardlink target %q", header.Linkname)
		}
		if err := stageParentDir(target); err != nil {
			return nil, err
		}
		source := filepath.Join(dst, filepath.FromSlash(linkCleaned))
		if err := os.Link(source, target); err != nil {
			return nil, fmt.Errorf("creating hardlink %s: %w", header.Name, err)
		}
		return nil, nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		slog.Warn("Skipping special file in volume export", "entry", header.Name, "typeflag", string(header.Typeflag))
		return nil, nil
	default:
		slog.Warn("Skipping unsupported tar entry", "entry", header.Name, "typeflag", string(header.Typeflag))
		return nil, nil
	}
}

// stageParentDir ensures the parent of an entry exists even if the archive
// omitted an explicit directory entry.
func stageParentDir(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("creating parent directory for %s: %w", target, err)
	}
	return nil
}

func writeTarFile(tr *tar.Reader, target string, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, tr)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// applyMetadata restores permissions and timestamps. Ownership is best
// effort: an unprivileged bailer cannot chown, and such runs record the
// staging user's ownership in the snapshot.
func applyMetadata(target string, header *tar.Header) error {
	if err := os.Chmod(target, header.FileInfo().Mode()); err != nil {
		return err
	}
	_ = os.Chown(target, header.Uid, header.Gid)
	modTime := header.ModTime
	accessTime := header.AccessTime
	if accessTime.IsZero() {
		accessTime = modTime
	}
	return os.Chtimes(target, accessTime, modTime)
}
