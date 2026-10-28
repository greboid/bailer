package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestValidateBackend(t *testing.T) {
	lookupOK := func(string) (string, error) { return "/usr/bin/restic", nil }
	lookupMissing := func(string) (string, error) { return "", errors.New("not found") }

	tests := []struct {
		name      string
		backend   string
		lookup    func(string) (string, error)
		env       map[string]string
		restic    string
		backupDir string
		expectErr string
	}{
		{"local backend valid", "local", lookupMissing, nil, "", "", ""},
		{"restic valid with repository file and password file", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY_FILE": "/repo", "RESTIC_PASSWORD_FILE": "/pw"}, "tar", "", ""},
		{"restic valid with repository and password", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "tar", "", ""},
		{"restic valid with password command", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD_COMMAND": "pass"}, "tar", "", ""},
		{"restic tar mode valid", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "tar", "", ""},
		{"restic files mode valid", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "files", "/backups", ""},
		{"restic files mode rejects root backup dir", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "files", "/", "backup directory"},
		{"restic unknown mode", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "borg", "/backups", "unknown restic mode"},
		{"restic missing binary", "restic", lookupMissing,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com", "RESTIC_PASSWORD": "x"}, "tar", "", "restic binary"},
		{"restic missing repository", "restic", lookupOK,
			map[string]string{"RESTIC_PASSWORD": "x"}, "tar", "", "RESTIC_REPOSITORY"},
		{"restic missing password", "restic", lookupOK,
			map[string]string{"RESTIC_REPOSITORY": "rest:https://example.com"}, "tar", "", "RESTIC_PASSWORD"},
		{"unknown backend", "borg", lookupOK, nil, "", "", "unknown backend"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := setResticEnv(t, tt.env)
			defer restore()

			err := validateBackend(tt.backend, "restic", tt.restic, tt.backupDir, tt.lookup)
			if tt.expectErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.expectErr)
			}
			if !strings.Contains(err.Error(), tt.expectErr) {
				t.Errorf("expected error containing %q, got: %v", tt.expectErr, err)
			}
		})
	}
}

// setResticEnv overrides the standard restic environment variables for the
// duration of a test and restores the previous values afterwards.
func setResticEnv(t *testing.T, env map[string]string) func() {
	t.Helper()
	keys := []string{"RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND"}
	previous := make(map[string]string, len(keys))
	for _, k := range keys {
		previous[k] = os.Getenv(k)
		value := env[k]
		if value == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, value)
		}
	}
	return func() {
		for _, k := range keys {
			if previous[k] == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, previous[k])
			}
		}
	}
}

func TestNewBackend(t *testing.T) {
	mc := &mockClient{}

	t.Run("local by default name", func(t *testing.T) {
		backend, err := newBackend("local", mc, "/backups", "restic", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := backend.(*LocalTarBackend); !ok {
			t.Fatalf("expected LocalTarBackend, got %T", backend)
		}
	})

	t.Run("restic resolves hostname and executable", func(t *testing.T) {
		backend, err := newBackend("restic", mc, "/backups", "restic", "docker-host-01", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		restic, ok := backend.(*ResticBackend)
		if !ok {
			t.Fatalf("expected ResticBackend, got %T", backend)
		}
		if restic.host != "docker-host-01" {
			t.Errorf("expected host docker-host-01, got %q", restic.host)
		}
		if restic.selfPath == "" {
			t.Error("expected selfPath to be resolved")
		}
		if restic.mode != resticModeTar {
			t.Errorf("expected default mode tar, got %q", restic.mode)
		}
		if restic.stagingDir != "/backups/staging" {
			t.Errorf("expected staging dir /backups/staging, got %q", restic.stagingDir)
		}
	})

	t.Run("restic files mode stages under the backup dir", func(t *testing.T) {
		backend, err := newBackend("restic", mc, "/backups", "restic", "host1", "files")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		restic, ok := backend.(*ResticBackend)
		if !ok {
			t.Fatalf("expected ResticBackend, got %T", backend)
		}
		if restic.mode != resticModeFiles {
			t.Errorf("expected mode files, got %q", restic.mode)
		}
		if restic.stagingDir != "/backups/staging" {
			t.Errorf("expected staging dir /backups/staging, got %q", restic.stagingDir)
		}
		if restic.selfPath != "" {
			t.Errorf("expected selfPath to be unset in files mode, got %q", restic.selfPath)
		}
	})

	t.Run("unknown backend rejected", func(t *testing.T) {
		if _, err := newBackend("borg", mc, "/backups", "restic", "", ""); err == nil {
			t.Fatal("expected error for unknown backend")
		}
	})
}

func TestResticBackupArgs(t *testing.T) {
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount: container.MountPoint{
			Type:        mount.TypeVolume,
			Name:        "appdata",
			Destination: "/data",
		},
	}

	args := resticBackupArgs("/bailer", "docker-host-01", request)

	expected := []string{
		"backup",
		"--host", "docker-host-01",
		"--tag", "bailer",
		"--tag", "container:myapp",
		"--tag", "volume:appdata",
		"--tag", "path:/data",
		"--stdin-filename", "myapp_appdata__data.tar",
		"--stdin-from-command",
		"--", "/bailer", "export-volume",
		"--container-id", "abc123",
		"--path", "/data",
	}
	if len(args) != len(expected) {
		t.Fatalf("unexpected arg count %d: %v", len(args), args)
	}
	for i := range expected {
		if args[i] != expected[i] {
			t.Errorf("args[%d] = %q, want %q (full: %v)", i, args[i], expected[i], args)
		}
	}
}

func TestResticBackupArgsBindMount(t *testing.T) {
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount: container.MountPoint{
			Type:        mount.TypeBind,
			Source:      "/srv/app/config",
			Destination: "/config",
		},
	}

	args := resticBackupArgs("/bailer", "host1", request)
	joined := strings.Join(args, "\x00")

	for _, want := range []string{"volume:/config", "path:/config", "myapp__config.tar", "--container-id\x00abc123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected args to contain %q, got: %v", want, args)
		}
	}
}

func TestStableArchiveName(t *testing.T) {
	tests := []struct {
		name     string
		request  BackupRequest
		expected string
	}{
		{
			"named volume",
			BackupRequest{ContainerName: "myapp", Mount: container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"}},
			"myapp_appdata__data.tar",
		},
		{
			"bind mount",
			BackupRequest{ContainerName: "myapp", Mount: container.MountPoint{Type: mount.TypeBind, Source: "/srv/data", Destination: "/data"}},
			"myapp__data.tar",
		},
		{
			"same volume mounted twice stays distinct",
			BackupRequest{ContainerName: "myapp", Mount: container.MountPoint{Type: mount.TypeVolume, Name: "shared", Destination: "/a"}},
			"myapp_shared__a.tar",
		},
		{
			"unsafe characters sanitised",
			BackupRequest{ContainerName: "my app", Mount: container.MountPoint{Type: mount.TypeVolume, Name: "vol:1", Destination: "/data base"}},
			"my_app_vol_1__data_base.tar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stableArchiveName(tt.request); got != tt.expected {
				t.Errorf("stableArchiveName() = %q, want %q", got, tt.expected)
			}
			if again := stableArchiveName(tt.request); again != tt.expected {
				t.Error("expected stable identity across calls")
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"simple", "simple"},
		{"with spaces and /slashes/", "with_spaces_and__slashes_"},
		{"dots.are-fine", "dots.are-fine"},
		{"un1codé", "un1cod_"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.input); got != tt.expected {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// writeScript creates an executable shell script, used to stub out external
// binaries in tests.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writing script %s: %v", name, err)
	}
	return path
}

func TestResticBackend_Success(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	resticBin := writeScript(t, binDir, "restic", `printf '%s\n' "$@" >> `+argsFile)

	backend := &ResticBackend{cli: &mockClient{}, resticBin: resticBin, host: "host1", selfPath: "/bailer"}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	if err := backend.Backup(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("restic was not invoked: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] != "backup" {
		t.Errorf("expected restic backup invocation, got: %v", lines)
	}
	if !strings.Contains(strings.Join(lines, " "), "--stdin-from-command") {
		t.Error("expected --stdin-from-command in restic arguments")
	}
}

func TestResticBackend_Failure(t *testing.T) {
	binDir := t.TempDir()
	resticBin := writeScript(t, binDir, "restic", `echo "repository is locked" >&2; exit 3`)

	backend := &ResticBackend{cli: &mockClient{}, resticBin: resticBin, host: "host1", selfPath: "/bailer"}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	err := backend.Backup(context.Background(), request)
	if err == nil {
		t.Fatal("expected error when restic fails")
	}
	if !strings.Contains(err.Error(), "restic backup of myapp_appdata__data.tar") {
		t.Errorf("expected archive identity in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("expected exit status in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "repository is locked") {
		t.Errorf("expected restic output in error, got: %v", err)
	}
}

func TestSlogWriter(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &slogWriter{logger: slog.Default(), level: slog.LevelDebug}
	for _, chunk := range []string{
		"using parent snapshot 67b1afeb\n",
		"\n",
		"progress 25%\rprogress 50%\rprogress done\n",
		"snapshot 74621bf1 saved",
	} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}
	w.Flush()

	logged := buf.String()
	for _, want := range []string{
		"using parent snapshot 67b1afeb",
		"progress 25%",
		"progress 50%",
		"progress done",
		"snapshot 74621bf1 saved",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected %q in log output, got:\n%s", want, logged)
		}
	}
	if strings.Count(logged, "\n") != 5 {
		t.Errorf("expected 5 log records (blank lines skipped), got:\n%s", logged)
	}

	if tail := w.tail(); !strings.Contains(tail, "snapshot 74621bf1 saved") {
		t.Errorf("expected captured output in tail, got %q", tail)
	}
}

func TestResticBackend_MissingBinary(t *testing.T) {
	backend := &ResticBackend{cli: &mockClient{}, resticBin: "/nonexistent/restic", host: "host1", selfPath: "/bailer"}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	if err := backend.Backup(context.Background(), request); err == nil {
		t.Fatal("expected error when restic binary is missing")
	}
}

func TestResticBackend_ContextCancellation(t *testing.T) {
	binDir := t.TempDir()
	// Restic stub that sleeps until it is killed; exec so the kill reaches
	// the sleeping process itself, as restic replaces itself cleanly.
	resticBin := writeScript(t, binDir, "restic", `exec sleep 30`)

	backend := &ResticBackend{cli: &mockClient{}, resticBin: resticBin, host: "host1", selfPath: "/bailer"}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- backend.Backup(ctx, request)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Backup did not return after context cancellation")
	}
}

// TestResticBackend_LocalArchiveAtomicity verifies the local backend never
// publishes an archive when the stream fails part-way through.
func TestLocalBackend_NoArchiveOnFailure(t *testing.T) {
	mc := &mockClient{
		copyErrs: map[string]error{"/data": errors.New("copy failed")},
	}
	backend := &LocalTarBackend{cli: mc, backupDir: t.TempDir()}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	if err := backend.Backup(context.Background(), request); err == nil {
		t.Fatal("expected error")
	}
	containerDir := filepath.Join(backend.backupDir, "myapp")
	entries, err := os.ReadDir(containerDir)
	if err != nil {
		t.Fatalf("reading backup dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz") {
			t.Errorf("published archive %q despite failure", entry.Name())
		}
	}
}

func TestLocalBackend_NoTempFilesLeftBehind(t *testing.T) {
	mc := &mockClient{}
	backend := &LocalTarBackend{cli: mc, backupDir: t.TempDir()}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	if err := backend.Backup(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	containerDir := filepath.Join(backend.backupDir, "myapp")
	entries, _ := os.ReadDir(containerDir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file after successful backup, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), ".tmp-") {
		t.Errorf("temporary file left behind: %q", entries[0].Name())
	}
}

// tarEntrySpec describes one entry of an in-memory tar archive built for
// tests.
type tarEntrySpec struct {
	name     string
	typeflag byte
	content  string
	mode     int64
	linkname string
	modTime  time.Time
}

func buildTar(t *testing.T, entries []tarEntrySpec) io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
			ModTime:  e.modTime,
			Size:     int64(len(e.content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header %s: %v", e.name, err)
		}
		if e.content != "" {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("writing tar content of %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

func TestResticFileBackupArgs(t *testing.T) {
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	args := resticFileBackupArgs("host1", "/staging/myapp_appdata__data/data", request)

	expected := []string{
		"backup",
		"--host", "host1",
		"--tag", "bailer",
		"--tag", "container:myapp",
		"--tag", "volume:appdata",
		"--tag", "path:/data",
		"/staging/myapp_appdata__data/data",
	}
	if len(args) != len(expected) {
		t.Fatalf("unexpected arg count %d: %v", len(args), args)
	}
	for i := range expected {
		if args[i] != expected[i] {
			t.Errorf("args[%d] = %q, want %q (full: %v)", i, args[i], expected[i], args)
		}
	}
}

func TestResticBackend_FilesMode_Success(t *testing.T) {
	binDir := t.TempDir()
	stagingDir := t.TempDir()
	copyDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	// Record the argv, then copy the staged tree aside so its contents can
	// be checked after backupFiles cleans it up.
	resticBin := writeScript(t, binDir, "restic",
		`printf '%s\n' "$@" >> `+argsFile+`
for last in "$@"; do :; done
cp -a "$last" `+copyDir+`/snapshot`)

	mc := &mockClient{customReader: buildTar(t, []tarEntrySpec{
		{name: "data/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/hello.txt", typeflag: tar.TypeReg, content: "hello", mode: 0o644},
	})}

	backend := &ResticBackend{cli: mc, resticBin: resticBin, host: "host1", mode: resticModeFiles, stagingDir: stagingDir}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	if err := backend.Backup(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mc.copyCalls) != 1 || mc.copyCalls[0].srcPath != "/data" {
		t.Errorf("expected one copyFromContainer call for /data, got: %v", mc.copyCalls)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("restic was not invoked: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	if args[0] != "backup" {
		t.Errorf("expected restic backup invocation, got: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--host host1", "--tag container:myapp", "--tag volume:appdata", "--tag path:/data"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected args to contain %q, got: %v", want, args)
		}
	}
	wantPath := filepath.Join(stagingDir, "myapp_appdata__data", "data")
	if args[len(args)-1] != wantPath {
		t.Errorf("expected restic target %q, got %q", wantPath, args[len(args)-1])
	}

	content, err := os.ReadFile(filepath.Join(copyDir, "snapshot", "hello.txt"))
	if err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
	if string(content) != "hello" {
		t.Errorf("unexpected staged content %q", content)
	}

	if _, err := os.Stat(filepath.Join(stagingDir, "myapp_appdata__data")); !os.IsNotExist(err) {
		t.Error("staging directory was not cleaned up after backup")
	}
}

func TestResticBackend_FilesMode_ResticFailure(t *testing.T) {
	binDir := t.TempDir()
	stagingDir := t.TempDir()
	resticBin := writeScript(t, binDir, "restic", `echo "repository is locked" >&2; exit 3`)

	mc := &mockClient{customReader: buildTar(t, []tarEntrySpec{
		{name: "data/hello.txt", typeflag: tar.TypeReg, content: "hello", mode: 0o644},
	})}

	backend := &ResticBackend{cli: mc, resticBin: resticBin, host: "host1", mode: resticModeFiles, stagingDir: stagingDir}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	err := backend.Backup(context.Background(), request)
	if err == nil {
		t.Fatal("expected error when restic fails")
	}
	if !strings.Contains(err.Error(), "restic backup of myapp_appdata__data") {
		t.Errorf("expected mount identity in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "repository is locked") {
		t.Errorf("expected restic output in error, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stagingDir, "myapp_appdata__data")); !os.IsNotExist(statErr) {
		t.Error("staging directory was not cleaned up after failure")
	}
}

func TestResticBackend_FilesMode_CopyError(t *testing.T) {
	stagingDir := t.TempDir()
	mc := &mockClient{
		copyErrs: map[string]error{"/data": errors.New("copy failed")},
	}

	backend := &ResticBackend{cli: mc, resticBin: "restic", host: "host1", mode: resticModeFiles, stagingDir: stagingDir}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	err := backend.Backup(context.Background(), request)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "copy failed") {
		t.Errorf("expected copy error, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stagingDir, "myapp_appdata__data")); !os.IsNotExist(statErr) {
		t.Error("staging directory should not exist after a copy error")
	}
}

func TestResticBackend_FilesMode_UnsafeArchiveEntry(t *testing.T) {
	binDir := t.TempDir()
	stagingDir := t.TempDir()
	outside := t.TempDir()
	resticBin := writeScript(t, binDir, "restic", `exit 1`)

	mc := &mockClient{customReader: buildTar(t, []tarEntrySpec{
		{name: "../escape.txt", typeflag: tar.TypeReg, content: "evil", mode: 0o644},
	})}

	backend := &ResticBackend{cli: mc, resticBin: resticBin, host: "host1", mode: resticModeFiles, stagingDir: stagingDir}
	request := BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount:         container.MountPoint{Type: mount.TypeVolume, Name: "appdata", Destination: "/data"},
	}

	err := backend.Backup(context.Background(), request)
	if err == nil {
		t.Fatal("expected error for unsafe tar entry")
	}
	if !strings.Contains(err.Error(), "refusing unsafe tar entry") {
		t.Errorf("expected refusal message, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(statErr) {
		t.Error("unsafe entry escaped the staging directory")
	}
	if _, statErr := os.Stat(filepath.Join(stagingDir, "myapp_appdata__data")); !os.IsNotExist(statErr) {
		t.Error("staging directory was not cleaned up after refusal")
	}
}

func TestExtractTarStream(t *testing.T) {
	reference := time.Date(2026, 4, 9, 14, 30, 1, 0, time.UTC)

	t.Run("dirs files and metadata", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/", typeflag: tar.TypeDir, mode: 0o750, modTime: reference},
			{name: "data/sub/", typeflag: tar.TypeDir, mode: 0o755, modTime: reference},
			{name: "data/sub/hello.txt", typeflag: tar.TypeReg, content: "hello", mode: 0o640, modTime: reference},
		}), dst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		info, err := os.Stat(filepath.Join(dst, "data", "sub", "hello.txt"))
		if err != nil {
			t.Fatalf("staged file missing: %v", err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Errorf("expected mode 0640, got %v", info.Mode().Perm())
		}
		if info.ModTime().Unix() != reference.Unix() {
			t.Errorf("expected mtime %v, got %v", reference, info.ModTime())
		}
		content, err := os.ReadFile(filepath.Join(dst, "data", "sub", "hello.txt"))
		if err != nil || string(content) != "hello" {
			t.Errorf("unexpected content %q (err: %v)", content, err)
		}

		subInfo, err := os.Stat(filepath.Join(dst, "data"))
		if err != nil {
			t.Fatalf("staged dir missing: %v", err)
		}
		if subInfo.Mode().Perm() != 0o750 {
			t.Errorf("expected dir mode 0750 after post-pass, got %v", subInfo.Mode().Perm())
		}
		if subInfo.ModTime().Unix() != reference.Unix() {
			t.Errorf("expected dir mtime %v, got %v (child writes clobbered it)", reference, subInfo.ModTime())
		}
	})

	t.Run("file before directory entry", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/sub/hello.txt", typeflag: tar.TypeReg, content: "hello", mode: 0o644},
			{name: "data/", typeflag: tar.TypeDir, mode: 0o755},
		}), dst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "data", "sub", "hello.txt")); err != nil {
			t.Errorf("file with implicit parent dirs missing: %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/link", typeflag: tar.TypeSymlink, linkname: "/etc/hostname", mode: 0o777},
		}), dst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		target, err := os.Readlink(filepath.Join(dst, "data", "link"))
		if err != nil {
			t.Fatalf("staged symlink missing: %v", err)
		}
		if target != "/etc/hostname" {
			t.Errorf("expected symlink target /etc/hostname, got %q", target)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/a.txt", typeflag: tar.TypeReg, content: "shared", mode: 0o644},
			{name: "data/b.txt", typeflag: tar.TypeLink, linkname: "data/a.txt", mode: 0o644},
		}), dst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		a, err := os.Stat(filepath.Join(dst, "data", "a.txt"))
		if err != nil {
			t.Fatalf("staged file missing: %v", err)
		}
		b, err := os.Stat(filepath.Join(dst, "data", "b.txt"))
		if err != nil {
			t.Fatalf("staged hardlink missing: %v", err)
		}
		if !os.SameFile(a, b) {
			t.Error("expected hardlink to share an inode with its target")
		}
	})

	t.Run("refuses escaping entries", func(t *testing.T) {
		for _, name := range []string{"../evil.txt", "/etc/evil.txt", "data/../../evil.txt"} {
			dst := t.TempDir()
			err := extractTarStream(buildTar(t, []tarEntrySpec{
				{name: name, typeflag: tar.TypeReg, content: "evil", mode: 0o644},
			}), dst)
			if err == nil {
				t.Errorf("expected error for entry %q", name)
				continue
			}
			if !strings.Contains(err.Error(), "refusing unsafe tar entry") {
				t.Errorf("entry %q: expected refusal message, got: %v", name, err)
			}
		}
	})

	t.Run("refuses escaping hardlink target", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/link", typeflag: tar.TypeLink, linkname: "../../../etc/passwd", mode: 0o644},
		}), dst)
		if err == nil || !strings.Contains(err.Error(), "refusing unsafe hardlink target") {
			t.Errorf("expected refusal of unsafe hardlink target, got: %v", err)
		}
	})

	t.Run("skips special files", func(t *testing.T) {
		dst := t.TempDir()
		err := extractTarStream(buildTar(t, []tarEntrySpec{
			{name: "data/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "data/dev", typeflag: tar.TypeChar, mode: 0o644},
			{name: "data/pipe", typeflag: tar.TypeFifo, mode: 0o644},
		}), dst)
		if err != nil {
			t.Fatalf("special files should be skipped, got error: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(dst, "data"))
		if err != nil {
			t.Fatalf("reading staged dir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected special files to be skipped, got: %v", entries)
		}
	})

	t.Run("handles truncated stream", func(t *testing.T) {
		dst := t.TempDir()
		full := buildTar(t, []tarEntrySpec{
			{name: "data/hello.txt", typeflag: tar.TypeReg, content: strings.Repeat("x", 3000), mode: 0o644},
		})
		data, err := io.ReadAll(full)
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}

		// Truncated inside the file content: the copy of the entry fails.
		err = extractTarStream(bytes.NewReader(data[:2000]), dst)
		if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Errorf("expected unexpected-EOF error for truncated content, got: %v", err)
		}

		// Truncated inside a header: the next-entry read fails.
		err = extractTarStream(bytes.NewReader(data[:300]), dst)
		if err == nil || !strings.Contains(err.Error(), "reading tar archive") {
			t.Errorf("expected archive read error for truncated header, got: %v", err)
		}
	})
}
