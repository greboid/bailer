//go:build integration

package main

// Restic integration tests using a real restic binary against a temporary
// local repository. These do not require a Docker daemon: the exporter is
// replaced by a helper binary that emits a real tar stream. Run with:
//
//	go test -tags integration -run TestIntegration_Restic ./...
//
// The restic binary is resolved from RESTIC_TEST_BIN or PATH; tests skip
// when it is not available.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

const testPassword = "integration-test-password"

func requireRestic(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RESTIC_TEST_BIN")
	if bin == "" {
		path, err := exec.LookPath("restic")
		if err != nil {
			t.Skip("restic binary not found (set RESTIC_TEST_BIN to enable)")
		}
		bin = path
	}
	return bin
}

// initResticRepo creates an initialised local restic repository and points
// the process environment at it.
func initResticRepo(t *testing.T, resticBin string) string {
	t.Helper()
	repoDir := t.TempDir()
	t.Setenv("RESTIC_REPOSITORY", repoDir)
	t.Setenv("RESTIC_PASSWORD", testPassword)

	out, err := exec.Command(resticBin, "init").CombinedOutput()
	if err != nil {
		t.Fatalf("restic init failed: %v\n%s", err, out)
	}
	return repoDir
}

func resticOutput(t *testing.T, resticBin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(resticBin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("restic %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

type resticSnapshot struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
}

func resticSnapshots(t *testing.T, resticBin string) []resticSnapshot {
	t.Helper()
	var snapshots []resticSnapshot
	if err := json.Unmarshal([]byte(resticOutput(t, resticBin, "snapshots", "--json")), &snapshots); err != nil {
		t.Fatalf("parsing snapshots: %v", err)
	}
	return snapshots
}

func resticDump(t *testing.T, resticBin, snapshotID, filename string) []byte {
	t.Helper()
	out, err := exec.Command(resticBin, "dump", snapshotID, filename).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("restic dump failed: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("restic dump failed: %v", err)
	}
	return out
}

func repoSize(t *testing.T, repoDir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(repoDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("measuring repository size: %v", err)
	}
	return total
}

// buildFakeExporter compiles a stand-in for the bailer export-volume
// subprocess that emits a real tar archive with directories, regular files
// with permissions, a symlink and a generation marker.
func buildFakeExporter(t *testing.T) string {
	t.Helper()

	source := `package main

import (
	"archive/tar"
	"fmt"
	"math/rand"
	"os"
	"time"
)

func main() {
	gen := os.Getenv("EXPORTER_GEN")
	if gen == "" {
		gen = "1"
	}

	writer := tar.NewWriter(os.Stdout)
	defer writer.Close()

	headers := []*tar.Header{
		{Name: "data/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(1700000000, 0)},
		{Name: "data/hello.txt", Mode: 0o640, Size: int64(len("hello world\n")), ModTime: time.Unix(1700000001, 0)},
		{Name: "data/sub/", Mode: 0o750, Typeflag: tar.TypeDir, ModTime: time.Unix(1700000002, 0)},
		{Name: "data/sub/nested.txt", Mode: 0o600, Size: int64(len("nested\n")), ModTime: time.Unix(1700000003, 0)},
	}

	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch header.Name {
		case "data/hello.txt":
			writer.Write([]byte("hello world\n"))
		case "data/sub/nested.txt":
			writer.Write([]byte("nested\n"))
		}
	}

	// Incompressible content so that dedup behaviour is measurable.
	big := make([]byte, 2<<20)
	rng := rand.New(rand.NewSource(42))
	rng.Read(big)
	if err := writer.WriteHeader(&tar.Header{Name: "data/big.bin", Mode: 0o600, Size: int64(len(big)), ModTime: time.Unix(1700000004, 0)}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writer.Write(big)

	if err := writer.WriteHeader(&tar.Header{Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "hello.txt", Mode: 0o777, ModTime: time.Unix(1700000005, 0)}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	genContent := fmt.Sprintf("generation %s\n", gen)
	if err := writer.WriteHeader(&tar.Header{Name: "data/gen.txt", Mode: 0o644, Size: int64(len(genContent)), ModTime: time.Unix(1700000006, 0)}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writer.Write([]byte(genContent))

	if os.Getenv("EXPORTER_SLOW") != "" {
		// Simulate a long-running export: pad with slow chunks.
		chunk := make([]byte, 64<<10)
		for i := 0; i < 100; i++ {
			time.Sleep(50 * time.Millisecond)
			if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("data/slow_%d.bin", i), Mode: 0o600, Size: int64(len(chunk)), ModTime: time.Unix(1700000007, 0)}); err != nil {
				break
			}
			writer.Write(chunk)
		}
	}

	if os.Getenv("EXPORTER_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "simulated exporter failure")
		os.Exit(1)
	}
}
`

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "exporter.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatalf("writing exporter source: %v", err)
	}
	binary := filepath.Join(dir, "exporter")
	build := exec.Command("go", "build", "-o", binary, sourcePath)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building exporter: %v\n%s", err, out)
	}
	return binary
}

func newResticTestBackend(t *testing.T, resticBin, exporterBin string) *ResticBackend {
	t.Helper()
	return &ResticBackend{
		cli:       &mockClient{},
		resticBin: resticBin,
		host:      "docker-host-01",
		selfPath:  exporterBin,
	}
}

func testRequest() BackupRequest {
	return BackupRequest{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Mount: container.MountPoint{
			Type:        mount.TypeVolume,
			Name:        "appdata",
			Destination: "/data",
		},
	}
}

func TestIntegration_Restic_SuccessfulBackup(t *testing.T) {
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin)
	backend := newResticTestBackend(t, resticBin, buildFakeExporter(t))

	if err := backend.Backup(context.Background(), testRequest()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Hostname != "docker-host-01" {
		t.Errorf("expected host docker-host-01, got %q", snapshot.Hostname)
	}
	for _, tag := range []string{"bailer", "container:myapp", "volume:appdata", "path:/data"} {
		if !contains(snapshot.Tags, tag) {
			t.Errorf("expected snapshot tags to contain %q, got %v", tag, snapshot.Tags)
		}
	}
}

func TestIntegration_Restic_IncrementalDedup(t *testing.T) {
	resticBin := requireRestic(t)
	repoDir := initResticRepo(t, resticBin)
	exporter := buildFakeExporter(t)
	backend := newResticTestBackend(t, resticBin, exporter)

	t.Setenv("EXPORTER_GEN", "1")
	if err := backend.Backup(context.Background(), testRequest()); err != nil {
		t.Fatalf("first backup failed: %v", err)
	}
	sizeAfterFirst := repoSize(t, repoDir)

	t.Setenv("EXPORTER_GEN", "2")
	if err := backend.Backup(context.Background(), testRequest()); err != nil {
		t.Fatalf("second backup failed: %v", err)
	}
	sizeAfterSecond := repoSize(t, repoDir)

	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}

	// The 2MiB incompressible file must be deduplicated: only the small
	// generation marker should add meaningful new data.
	growth := sizeAfterSecond - sizeAfterFirst
	if growth > 1<<20 {
		t.Errorf("expected deduplication, repository grew by %d bytes", growth)
	}

	dumped := resticDump(t, resticBin, "latest", "myapp_appdata__data.tar")
	if !strings.Contains(string(dumped), "generation 2") {
		t.Error("expected latest snapshot to contain generation 2 content")
	}
}

func TestIntegration_Restic_FailedExporterNoSnapshot(t *testing.T) {
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin)
	exporter := buildFakeExporter(t)
	backend := newResticTestBackend(t, resticBin, exporter)

	// The exporter writes megabytes of data before failing, so restic has
	// received a partial stream when the child exits non-zero.
	t.Setenv("EXPORTER_FAIL", "1")
	err := backend.Backup(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected error when exporter fails")
	}

	if snapshots := resticSnapshots(t, resticBin); len(snapshots) != 0 {
		t.Fatalf("expected no snapshot from failed export, got %d", len(snapshots))
	}
}

func TestIntegration_Restic_AuthFailure(t *testing.T) {
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin) // initialised with testPassword
	backend := newResticTestBackend(t, resticBin, buildFakeExporter(t))

	t.Setenv("RESTIC_PASSWORD", "wrong-password")
	if err := backend.Backup(context.Background(), testRequest()); err == nil {
		t.Fatal("expected error when repository authentication fails")
	}
}

func TestIntegration_Restic_ExporterTerminatedOnCancel(t *testing.T) {
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin)
	exporter := buildFakeExporter(t)
	backend := newResticTestBackend(t, resticBin, exporter)

	t.Setenv("EXPORTER_SLOW", "1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- backend.Backup(ctx, testRequest())
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after cancellation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Backup did not return after cancellation")
	}

	// The exporter must not outlive the cancelled backup.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("pgrep", "-f", exporter).Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exporter still running after cancellation: %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if snapshots := resticSnapshots(t, resticBin); len(snapshots) != 0 {
		t.Errorf("expected no snapshot after cancellation, got %d", len(snapshots))
	}
}

func TestIntegration_Restic_RestoreFromDump(t *testing.T) {
	resticBin := requireRestic(t)
	initResticRepo(t, resticBin)
	backend := newResticTestBackend(t, resticBin, buildFakeExporter(t))

	if err := backend.Backup(context.Background(), testRequest()); err != nil {
		t.Fatalf("backup failed: %v", err)
	}
	snapshots := resticSnapshots(t, resticBin)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}

	dumped := resticDump(t, resticBin, snapshots[0].ID, "myapp_appdata__data.tar")

	reader := tar.NewReader(bytes.NewReader(dumped))
	type entry struct {
		name     string
		mode     int64
		typeflag byte
		linkname string
		content  string
	}
	var entries []entry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading dumped tar: %v", err)
		}
		e := entry{name: header.Name, mode: header.Mode, typeflag: header.Typeflag, linkname: header.Linkname}
		if header.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(reader)
			e.content = string(data)
		}
		entries = append(entries, e)
	}

	findEntry := func(name string) *entry {
		for i := range entries {
			if entries[i].name == name {
				return &entries[i]
			}
		}
		return nil
	}

	hello := findEntry("data/hello.txt")
	if hello == nil {
		t.Fatalf("expected data/hello.txt in archive, got entries: %v", entries)
	}
	if hello.mode != 0o640 {
		t.Errorf("expected mode 0640, got %#o", hello.mode)
	}
	if hello.content != "hello world\n" {
		t.Errorf("unexpected content: %q", hello.content)
	}

	link := findEntry("data/link")
	if link == nil || link.typeflag != tar.TypeSymlink || link.linkname != "hello.txt" {
		t.Errorf("expected symlink data/link -> hello.txt, got %+v", link)
	}

	nested := findEntry("data/sub/nested.txt")
	if nested == nil || nested.mode != 0o600 {
		t.Errorf("expected data/sub/nested.txt with mode 0600, got %+v", nested)
	}
}
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
