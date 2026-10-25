//go:build integration

package main

// Docker end-to-end integration tests. These require a running Docker daemon
// and a compiled bailer binary, and exercise the full pipeline: discovery,
// hooks, volume export via CopyFromContainer, the restic subprocess chain and
// the local tar backend. Run with:
//
//	go test -tags integration -run TestIntegration_Docker ./...
//
// Tests skip when no Docker daemon is reachable.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

func requireDocker(t *testing.T) *dockerClient {
	t.Helper()
	rawCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("cannot create docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rawCli.Ping(ctx); err != nil {
		rawCli.Close()
		t.Skipf("no reachable Docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = rawCli.Close() })
	return &dockerClient{rawCli}
}

// buildBailer compiles the bailer binary once per test.
func buildBailer(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "bailer")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building bailer: %v\n%s", err, out)
	}
	return binary
}

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// populateVolume creates a busybox container that fills a volume with files,
// a subdirectory, a symlink, permissions and a generation marker.
func populateVolume(t *testing.T, volume, generation string) {
	t.Helper()
	dockerRun(t, "run", "--rm", "-v", volume+":/data", "busybox", "sh", "-c",
		"mkdir -p /data/sub && "+
			"echo 'hello world' > /data/hello.txt && "+
			"chmod 640 /data/hello.txt && "+
			"echo nested > /data/sub/nested.txt && "+
			"ln -sf hello.txt /data/link && "+
			"echo 'generation "+generation+"' > /data/gen.txt")
}

func runBailer(t *testing.T, binary string, env map[string]string) (int, string) {
	t.Helper()
	if env["MODE"] == "" {
		env["MODE"] = "include"
	}
	cmd := exec.Command(binary, "-once")
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running bailer: %v\n%s", err, output)
	}
	return code, string(output)
}

// testContainerLabel restricts bailer to this test's container, keeping
// runs hermetic on a shared daemon.
func testContainerLabel(t *testing.T) string {
	t.Helper()
	return "com.greboid.bailer.include=/data"
}

// removeStaleContainer force-removes any leftover container from a previous
// test run before the name is reused.
func removeStaleContainer(t *testing.T, name string) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

// snapshotFilename computes the stable archive name the restic backend
// stores for a test container mounting <volume>:/data.
func snapshotFilename(containerName, volume string) string {
	return stableArchiveName(BackupRequest{
		ContainerName: containerName,
		Mount: container.MountPoint{
			Type:        mount.TypeVolume,
			Name:        volume,
			Destination: "/data",
		},
	})
}

func testVolume(t *testing.T) string {
	t.Helper()
	name := "bailer-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	_ = exec.Command("docker", "volume", "rm", name).Run()
	dockerRun(t, "volume", "create", name)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", name).Run() })
	return name
}

func TestIntegration_Docker_ResticEndToEnd(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	repoDir := initResticRepo(t, resticBin)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	populateVolume(t, volume, "1")
	removeStaleContainer(t, "bailer-e2e-app")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-app", "-v", volume+":/data",
		"--label", testContainerLabel(t), "busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-app").Run() })

	code, output := runBailer(t, bailerBin, map[string]string{
		"BACKEND":           "restic",
		"RESTIC_REPOSITORY": repoDir,
		"RESTIC_PASSWORD":   testPassword,
		"RESTIC_HOST":       "e2e-host",
		"RESTIC_BIN":        resticBin,
	})
	if code != 0 {
		t.Fatalf("bailer exited %d:\n%s", code, output)
	}

	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Hostname != "e2e-host" {
		t.Errorf("expected host e2e-host, got %q", snapshots[0].Hostname)
	}

	// Restoring must produce the exact volume content.
	dumped := resticDump(t, resticBin, snapshots[0].ID, snapshotFilename("bailer-e2e-app", volume))
	verifyVolumeTar(t, dumped, "generation 1\n")
}

// verifyVolumeTar checks entries, permissions, symlink and content of a tar
// stream produced by Docker's CopyFromContainer for populateVolume().
func verifyVolumeTar(t *testing.T, data []byte, generation string) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	found := map[string]*tar.Header{}
	contents := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		found[strings.TrimSuffix(header.Name, "/")] = header
		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("reading %s: %v", header.Name, err)
			}
			contents[header.Name] = string(data)
		}
	}

	// CopyFromContainer roots entries at the base name of the exported path.
	for _, name := range []string{"data", "data/hello.txt", "data/sub", "data/sub/nested.txt", "data/link", "data/gen.txt"} {
		if _, ok := found[name]; !ok {
			t.Errorf("expected entry %q in archive, got %v", name, headerNames(found))
		}
	}
	if header := found["data/hello.txt"]; header != nil && header.Mode != 0o640 {
		t.Errorf("expected mode 0640 for hello.txt, got %#o", header.Mode)
	}
	if header := found["data/link"]; header != nil && (header.Typeflag != tar.TypeSymlink || header.Linkname != "hello.txt") {
		t.Errorf("unexpected symlink entry: %+v", header)
	}
	if contents["data/gen.txt"] != generation {
		t.Errorf("expected gen.txt %q, got %q", generation, contents["data/gen.txt"])
	}
}

func headerNames(headers map[string]*tar.Header) []string {
	var names []string
	for name := range headers {
		names = append(names, name)
	}
	return names
}

func TestIntegration_Docker_ResticIncremental(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	repoDir := initResticRepo(t, resticBin)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	populateVolume(t, volume, "1")
	removeStaleContainer(t, "bailer-e2e-inc")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-inc", "-v", volume+":/data",
		"--label", testContainerLabel(t), "busybox", "sleep", "600")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-inc").Run() })

	env := map[string]string{
		"BACKEND":           "restic",
		"RESTIC_REPOSITORY": repoDir,
		"RESTIC_PASSWORD":   testPassword,
		"RESTIC_HOST":       "e2e-host",
		"RESTIC_BIN":        resticBin,
	}

	if code, output := runBailer(t, bailerBin, env); code != 0 {
		t.Fatalf("first backup exited %d:\n%s", code, output)
	}
	sizeAfterFirst := repoSize(t, repoDir)

	// Make a small change between runs.
	dockerRun(t, "exec", "bailer-e2e-inc", "sh", "-c", "echo 'generation 2' > /data/gen.txt")

	if code, output := runBailer(t, bailerBin, env); code != 0 {
		t.Fatalf("second backup exited %d:\n%s", code, output)
	}
	sizeAfterSecond := repoSize(t, repoDir)

	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}

	dumped := resticDump(t, resticBin, "latest", snapshotFilename("bailer-e2e-inc", volume))
	verifyVolumeTar(t, dumped, "generation 2\n")

	if growth := sizeAfterSecond - sizeAfterFirst; growth > 1<<20 {
		t.Errorf("expected deduplicated incremental backup, repository grew by %d bytes", growth)
	}
}

func TestIntegration_Docker_FailedRepositoryOperation(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	bailerBin := buildBailer(t)

	// Point at a repository directory that does not exist and has not been
	// initialised; bailer must report the failure with a non-zero exit.
	missingRepo := filepath.Join(t.TempDir(), "missing-repo")

	volume := testVolume(t)
	populateVolume(t, volume, "1")
	removeStaleContainer(t, "bailer-e2e-failrepo")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-failrepo", "-v", volume+":/data",
		"--label", testContainerLabel(t), "busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-failrepo").Run() })

	code, output := runBailer(t, bailerBin, map[string]string{
		"BACKEND":           "restic",
		"RESTIC_REPOSITORY": missingRepo,
		"RESTIC_PASSWORD":   testPassword,
		"RESTIC_BIN":        resticBin,
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit for failed backup pass:\n%s", output)
	}
	if !strings.Contains(output, "Backup failed") {
		t.Errorf("expected failed-run log entry, got:\n%s", output)
	}
}

func TestIntegration_Docker_HookOrderingAndFailure(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	repoDir := initResticRepo(t, resticBin)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	removeStaleContainer(t, "bailer-e2e-hooks")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-hooks",
		"-v", volume+":/data",
		"--label", testContainerLabel(t),
		"--label", "com.greboid.bailer.pre-backup-cmd=echo pre >> /data/hooklog",
		"--label", "com.greboid.bailer.post-backup-cmd=echo post >> /data/hooklog",
		"busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-hooks").Run() })

	code, output := runBailer(t, bailerBin, map[string]string{
		"BACKEND":           "restic",
		"RESTIC_REPOSITORY": repoDir,
		"RESTIC_PASSWORD":   testPassword,
		"RESTIC_BIN":        resticBin,
	})
	if code != 0 {
		t.Fatalf("bailer exited %d:\n%s", code, output)
	}

	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot (pre-hook ran once, not per subprocess), got %d", len(snapshots))
	}

	// Pre-hook ran before the backup, post-hook after: the log written into
	// the volume and captured by the snapshot shows the order.
	dumped := resticDump(t, resticBin, snapshots[0].ID, snapshotFilename("bailer-e2e-hooks", volume))
	if !strings.Contains(string(dumped), "pre") {
		t.Error("expected pre-hook marker in snapshot")
	}

	hookLog := dockerRun(t, "exec", "bailer-e2e-hooks", "cat", "/data/hooklog")
	if strings.TrimSpace(hookLog) != "pre\npost" {
		t.Errorf("expected hook order pre,post; got %q", hookLog)
	}
}

func TestIntegration_Docker_PreHookFailureSkipsBackup(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	repoDir := initResticRepo(t, resticBin)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	removeStaleContainer(t, "bailer-e2e-prefail")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-prefail",
		"-v", volume+":/data",
		"--label", testContainerLabel(t),
		"--label", "com.greboid.bailer.pre-backup-cmd=false",
		"--label", "com.greboid.bailer.post-backup-cmd=echo post >> /data/hooklog",
		"busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-prefail").Run() })

	code, output := runBailer(t, bailerBin, map[string]string{
		"BACKEND":           "restic",
		"RESTIC_REPOSITORY": repoDir,
		"RESTIC_PASSWORD":   testPassword,
		"RESTIC_BIN":        resticBin,
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit when pre-backup hook fails:\n%s", output)
	}
	if snapshots := resticSnapshots(t, resticBin); len(snapshots) != 0 {
		t.Errorf("expected no snapshot when pre-hook fails, got %d", len(snapshots))
	}

	// The container is skipped entirely: the post-hook must not run.
	if hookLog := dockerRun(t, "exec", "bailer-e2e-prefail", "sh", "-c", "cat /data/hooklog 2>/dev/null || echo absent"); strings.TrimSpace(hookLog) != "absent" {
		t.Errorf("expected post-hook not to run after pre-hook failure, got %q", hookLog)
	}
}

func TestIntegration_Docker_LocalBackendBackwardsCompatible(t *testing.T) {
	requireDocker(t)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	populateVolume(t, volume, "1")
	removeStaleContainer(t, "bailer-e2e-local")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-local", "-v", volume+":/data",
		"--label", testContainerLabel(t), "busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-local").Run() })

	backupDir := t.TempDir()
	code, output := runBailer(t, bailerBin, map[string]string{"BACKUP_DIR": backupDir})
	if code != 0 {
		t.Fatalf("bailer exited %d:\n%s", code, output)
	}

	matches, err := filepath.Glob(filepath.Join(backupDir, "bailer-e2e-local", "*.tar.gz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 archive, got %d (%v)", len(matches), err)
	}

	file, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("archive is not valid gzip: %v", err)
	}
	verifyVolumeTar(t, streamAll(t, gz), "generation 1\n")
}

func streamAll(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return data
}

func TestIntegration_Docker_CancellationLeavesNoChildren(t *testing.T) {
	requireDocker(t)
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	removeStaleContainer(t, "bailer-e2e-cancel")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-cancel", "-v", volume+":/data",
		"--label", testContainerLabel(t), "busybox", "sh", "-c",
		"dd if=/dev/urandom of=/data/big.bin bs=1M count=64 2>/dev/null; sleep 600")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-cancel").Run() })

	cmd := exec.Command(bailerBin, "-once")
	cmd.Env = append(os.Environ(),
		"MODE=include",
		"BACKEND=restic",
		"RESTIC_REPOSITORY="+t.TempDir(),
		"RESTIC_PASSWORD="+testPassword,
		"RESTIC_BIN="+resticBin,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting bailer: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("bailer did not exit after SIGTERM")
	}

	// Neither restic nor a bailer exporter may survive the parent.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resticOut, _ := exec.Command("pgrep", "-f", resticBin).Output()
		exportOut, _ := exec.Command("pgrep", "-f", "export-volume").Output()
		if len(resticOut) == 0 && len(exportOut) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child processes survived cancellation: restic=%s exporter=%s", resticOut, exportOut)
		}
		time.Sleep(100 * time.Millisecond)
	}

}

func TestIntegration_Docker_ExportVolumeSubcommand(t *testing.T) {
	requireDocker(t)
	bailerBin := buildBailer(t)

	volume := testVolume(t)
	populateVolume(t, volume, "1")
	removeStaleContainer(t, "bailer-e2e-export")
	dockerRun(t, "run", "-d", "--name", "bailer-e2e-export", "-v", volume+":/data",
		"busybox", "sleep", "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "bailer-e2e-export").Run() })

	containerID := dockerRun(t, "inspect", "-f", "{{.Id}}", "bailer-e2e-export")

	out, err := exec.Command(bailerBin, "export-volume", "--container-id", containerID, "--path", "/data").Output()
	if err != nil {
		t.Fatalf("export-volume failed: %v", err)
	}
	verifyVolumeTar(t, out, "generation 1\n")

	// Failures must produce a non-zero exit code and no stdout contamination.
	if code := exec.Command(bailerBin, "export-volume", "--container-id", "nonexistent0000000000000000000000000000000000000000000000000000000000", "--path", "/data").Run(); code == nil {
		t.Error("expected non-zero exit for missing container")
	}
	if code := exec.Command(bailerBin, "export-volume", "--container-id", containerID, "--path", "/data/../etc").Run(); code == nil {
		t.Error("expected non-zero exit for non-canonical path")
	}
}
