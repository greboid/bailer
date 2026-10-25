package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
)

func (m *mockClient) imageInspect(_ context.Context, ref string) (image.InspectResponse, error) {
	m.inspectCalls = append(m.inspectCalls, ref)
	if m.inspectErr != nil {
		return image.InspectResponse{}, m.inspectErr
	}
	if m.imagesPresent[ref] {
		return image.InspectResponse{ID: "sha256:" + ref}, nil
	}
	return image.InspectResponse{}, fmt.Errorf("No such image: %s: %w", ref, errdefs.ErrNotFound)
}

func (m *mockClient) imagePull(_ context.Context, ref string) (io.ReadCloser, error) {
	m.pullCalls = append(m.pullCalls, ref)
	if m.pullErr != nil {
		return nil, m.pullErr
	}
	m.imagesPresent[ref] = true
	return io.NopCloser(strings.NewReader("{}")), nil
}

func (m *mockClient) containerCreate(_ context.Context, config container.Config, hostConfig container.HostConfig) (container.CreateResponse, error) {
	if m.createErr != nil {
		return container.CreateResponse{}, m.createErr
	}
	m.createCalls = append(m.createCalls, createCall{
		image:      config.Image,
		entrypoint: config.Entrypoint,
		user:       config.User,
		mounts:     hostConfig.Mounts,
	})
	return container.CreateResponse{ID: "helper123456789"}, nil
}

func (m *mockClient) containerStart(_ context.Context, containerID string) error {
	m.startCalls = append(m.startCalls, containerID)
	return m.startErr
}

func (m *mockClient) containerWait(_ context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	m.waitCalls = append(m.waitCalls, waitCall{containerID, condition})
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	if m.waitErr != nil {
		errCh <- m.waitErr
	} else {
		statusCh <- container.WaitResponse{StatusCode: m.waitExitCode}
	}
	return statusCh, errCh
}

func (m *mockClient) containerRemove(_ context.Context, containerID string, options container.RemoveOptions) error {
	m.removeCalls = append(m.removeCalls, containerID)
	return m.removeErr
}

type waitCall struct {
	containerID string
	condition   container.WaitCondition
}

func helperTestMounts() []container.MountPoint {
	return []container.MountPoint{
		{Type: mount.TypeVolume, Name: "data", Source: "data", Destination: "/data", RW: true},
		{Type: mount.TypeBind, Source: "/host/config", Destination: "/config", RW: true},
		{Type: mount.TypeBind, Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
		{Type: mount.TypeTmpfs, Destination: "/tmp", RW: true},
	}
}

func runningContainerWithHelperLabels(labels map[string]string) container.Summary {
	return container.Summary{
		ID:     "running123456789",
		State:  "running",
		Names:  []string{"/myapp"},
		Labels: labels,
		Mounts: helperTestMounts(),
	}
}

func TestParseArgvLabel(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expected  []string
		expectErr bool
	}{
		{"simple argv", `["/usr/bin/sqlite3","-bail","db"]`, []string{"/usr/bin/sqlite3", "-bail", "db"}, false},
		{"single element", `["/bin/true"]`, []string{"/bin/true"}, false},
		{"arguments with spaces", `["/bin/tool","--flag","a b"]`, []string{"/bin/tool", "--flag", "a b"}, false},
		{"not json", "just a string", nil, true},
		{"json object", `{"cmd":"x"}`, nil, true},
		{"empty array", `[]`, nil, true},
		{"empty string", "", nil, true},
		{"array with empty argument", `["/bin/tool",""]`, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseArgvLabel(tt.input)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected error for input %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result) != len(tt.expected) {
				t.Fatalf("parseArgvLabel(%q) = %v, want %v", tt.input, result, tt.expected)
			}
			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("parseArgvLabel(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
				}
			}
		})
	}
}

func TestHelperMounts(t *testing.T) {
	result := helperMounts(helperTestMounts())
	if len(result) != 2 {
		t.Fatalf("expected 2 mounts, got %d: %v", len(result), result)
	}

	vol := result[0]
	if vol.Type != mount.TypeVolume || vol.Source != "data" || vol.Target != "/data" {
		t.Errorf("unexpected volume mount: %+v", vol)
	}
	if vol.ReadOnly {
		t.Error("expected volume mount to be read-write")
	}

	bind := result[1]
	if bind.Type != mount.TypeBind || bind.Source != "/host/config" || bind.Target != "/config" {
		t.Errorf("unexpected bind mount: %+v", bind)
	}
	if bind.ReadOnly {
		t.Error("expected bind mount to be read-write")
	}
}

func TestRunHelper_Success(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{"registry.example/helper:latest": true}}
	argv := `["/usr/bin/sqlite3","-bail","/data/db.sqlite3",".backup /data/db.bak"]`

	err := runHelper(context.Background(), mc, "myapp", "registry.example/helper:latest", argv, helperTestMounts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mc.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(mc.createCalls))
	}
	created := mc.createCalls[0]
	if created.image != "registry.example/helper:latest" {
		t.Errorf("unexpected image: %q", created.image)
	}
	expectedArgv := []string{"/usr/bin/sqlite3", "-bail", "/data/db.sqlite3", ".backup /data/db.bak"}
	if len(created.entrypoint) != len(expectedArgv) {
		t.Fatalf("unexpected entrypoint: %v", created.entrypoint)
	}
	for i := range expectedArgv {
		if created.entrypoint[i] != expectedArgv[i] {
			t.Errorf("entrypoint[%d] = %q, want %q", i, created.entrypoint[i], expectedArgv[i])
		}
	}
	if created.user != "0" {
		t.Errorf("expected user \"0\", got %q", created.user)
	}
	if len(created.mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d: %v", len(created.mounts), created.mounts)
	}

	if len(mc.startCalls) != 1 || mc.startCalls[0] != "helper123456789" {
		t.Errorf("expected start of helper container, got %v", mc.startCalls)
	}
	if len(mc.waitCalls) != 1 || mc.waitCalls[0].condition != container.WaitConditionNotRunning {
		t.Errorf("expected wait with WaitConditionNotRunning, got %v", mc.waitCalls)
	}
	if len(mc.removeCalls) != 1 || mc.removeCalls[0] != "helper123456789" {
		t.Errorf("expected remove of helper container, got %v", mc.removeCalls)
	}
	if len(mc.pullCalls) != 0 {
		t.Errorf("expected no pull when image is present, got %v", mc.pullCalls)
	}
}

func TestRunHelper_NonZeroExitReturnsErrorAndRemoves(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}, waitExitCode: 5}

	err := runHelper(context.Background(), mc, "myapp", "helper:latest", `["/bin/false"]`, nil)
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "code 5") || !strings.Contains(err.Error(), "myapp") {
		t.Errorf("error should mention exit code and container name, got: %v", err)
	}
	if len(mc.removeCalls) != 1 {
		t.Errorf("expected helper container to be removed, got %v", mc.removeCalls)
	}
}

func TestRunHelper_WaitErrorRemovesContainer(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}, waitErr: errors.New("wait failed")}

	err := runHelper(context.Background(), mc, "myapp", "helper:latest", `["/bin/true"]`, nil)
	if err == nil {
		t.Fatal("expected error on wait failure")
	}
	if len(mc.removeCalls) != 1 {
		t.Errorf("expected helper container to be removed, got %v", mc.removeCalls)
	}
}

func TestRunHelper_InvalidJSON(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}

	err := runHelper(context.Background(), mc, "myapp", "helper:latest", "not json", nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if len(mc.createCalls) != 0 {
		t.Errorf("expected no container creation, got %v", mc.createCalls)
	}
	if len(mc.removeCalls) != 0 {
		t.Errorf("expected no remove without creation, got %v", mc.removeCalls)
	}
}

func TestRunHelper_PullsMissingImage(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}

	err := runHelper(context.Background(), mc, "myapp", "helper:latest", `["/bin/true"]`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.pullCalls) != 1 || mc.pullCalls[0] != "helper:latest" {
		t.Errorf("expected pull of helper:latest, got %v", mc.pullCalls)
	}
	if len(mc.createCalls) != 1 {
		t.Errorf("expected creation after pull, got %v", mc.createCalls)
	}
}

func TestRunHelper_PullFailure(t *testing.T) {
	mc := &mockClient{
		imagesPresent: map[string]bool{},
		pullErr:       errors.New("pull failed"),
	}

	err := runHelper(context.Background(), mc, "myapp", "helper:latest", `["/bin/true"]`, nil)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
	if len(mc.createCalls) != 0 {
		t.Errorf("expected no container creation after failed pull, got %v", mc.createCalls)
	}
}

func TestRun_HelperPreBackupCommand(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperImage:   "git.mouse-lake.ts.net/containers/sqlite3:latest",
			labelHelperPreCmd:  `["/usr/bin/sqlite3",".backup /data/db.bak"]`,
			labelHelperPostCmd: `["/usr/bin/rm","/data/db.bak"]`,
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.createCalls) != 2 {
		t.Fatalf("expected 2 helper runs (pre and post), got %d", len(mc.createCalls))
	}
	if len(mc.execCalls) != 0 {
		t.Errorf("expected no exec calls, got %v", mc.execCalls)
	}
	if len(mc.copyCalls) != 2 {
		t.Errorf("expected 2 copy calls between helper commands, got %d", len(mc.copyCalls))
	}
	if len(mc.removeCalls) != 2 {
		t.Errorf("expected both helper containers removed, got %v", mc.removeCalls)
	}
}

func TestRun_HelperPreCmdOverridesExecCmd(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperImage:   "helper:latest",
			labelHelperPreCmd:  `["/bin/pre"]`,
			labelPreBackupCmd:  "exec_pre",
			labelHelperPostCmd: `["/bin/post"]`,
			labelPostBackupCmd: "exec_post",
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 0 {
		t.Errorf("expected exec to be skipped when helper commands set, got %v", mc.execCalls)
	}
	if len(mc.createCalls) != 2 {
		t.Errorf("expected helper pre and post to run, got %d creates", len(mc.createCalls))
	}
}

func TestRun_HelperPreWithoutImageSkipsContainer(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperPreCmd: `["/bin/pre"]`,
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err == nil {
		t.Fatal("expected error when helper image is missing")
	}
	if !strings.Contains(err.Error(), "no helper image") {
		t.Errorf("expected aggregated helper image failure, got: %v", err)
	}
	if len(mc.createCalls) != 0 {
		t.Errorf("expected no helper creation, got %v", mc.createCalls)
	}
	if len(mc.copyCalls) != 0 {
		t.Error("expected container to be skipped when helper image is missing")
	}
}

func TestRun_HelperPostWithoutImageLogsButBacksUp(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperPostCmd: `["/bin/post"]`,
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err == nil {
		t.Fatal("expected error when helper image is missing")
	}
	if !strings.Contains(err.Error(), "no helper image") {
		t.Errorf("expected aggregated helper image failure, got: %v", err)
	}
	if len(mc.copyCalls) != 2 {
		t.Errorf("expected backup to still run, got %d copy calls", len(mc.copyCalls))
	}
	if len(mc.createCalls) != 0 {
		t.Errorf("expected no helper creation, got %v", mc.createCalls)
	}
}

func TestRun_HelperPreFailureSkipsContainer(t *testing.T) {
	mc := &mockClient{
		imagesPresent: map[string]bool{},
		waitExitCode:  1,
	}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperImage:  "helper:latest",
			labelHelperPreCmd: `["/bin/fail"]`,
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err == nil {
		t.Fatal("expected error when helper pre-backup fails")
	}
	if !strings.Contains(err.Error(), "helper pre-backup command") {
		t.Errorf("expected aggregated helper pre-backup failure, got: %v", err)
	}
	if len(mc.copyCalls) != 0 {
		t.Error("expected container to be skipped when helper pre-backup fails")
	}
}

func TestRun_HelperPostFailureDoesNotBlock(t *testing.T) {
	mc := &mockClient{
		imagesPresent: map[string]bool{},
		waitExitCode:  1,
	}
	mc.containers = []container.Summary{
		runningContainerWithHelperLabels(map[string]string{
			labelHelperImage:   "helper:latest",
			labelHelperPostCmd: `["/bin/fail"]`,
		}),
	}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err == nil {
		t.Fatal("expected error when helper post-backup fails")
	}
	if !strings.Contains(err.Error(), "helper post-backup command") {
		t.Errorf("expected aggregated helper post-backup failure, got: %v", err)
	}
	if len(mc.copyCalls) != 2 {
		t.Error("expected backup to complete even when helper post-backup fails")
	}
}

func TestRun_HelperSkippedForStoppedContainer(t *testing.T) {
	mc := &mockClient{imagesPresent: map[string]bool{}}
	summary := runningContainerWithHelperLabels(map[string]string{
		labelHelperImage:   "helper:latest",
		labelHelperPreCmd:  `["/bin/pre"]`,
		labelHelperPostCmd: `["/bin/post"]`,
	})
	summary.State = "stopped"
	mc.containers = []container.Summary{summary}

	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.createCalls) != 0 {
		t.Errorf("expected no helper runs for stopped container, got %d", len(mc.createCalls))
	}
	if len(mc.copyCalls) != 2 {
		t.Errorf("expected backup of stopped container, got %d", len(mc.copyCalls))
	}
}
