package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
)

type mockClient struct {
	containers    []container.Summary
	listErr       error
	copyCalls     []copyCall
	execCalls     []execCall
	execErrs      map[string]error
	copyErrs      map[string]error
	imagesPresent map[string]bool
	inspectErr    error
	pullErr       error
	createErr     error
	startErr      error
	waitErr       error
	waitExitCode  int64
	removeErr     error
	logsErr       error
	logsOutput    string
	inspectCalls  []string
	pullCalls     []string
	createCalls   []createCall
	startCalls    []string
	waitCalls     []waitCall
	removeCalls   []string
	logsCalls     []string
	customReader  io.ReadCloser
}

type copyCall struct {
	containerID string
	srcPath     string
}

type execCall struct {
	containerID string
	user        string
	cmd         string
}

type createCall struct {
	image      string
	entrypoint []string
	user       string
	mounts     []mount.Mount
}

func (m *mockClient) listContainers(_ context.Context) ([]container.Summary, error) {
	return m.containers, m.listErr
}

func (m *mockClient) copyFromContainer(_ context.Context, containerID, srcPath string) (io.ReadCloser, error) {
	m.copyCalls = append(m.copyCalls, copyCall{containerID, srcPath})
	if err := m.copyErrs[srcPath]; err != nil {
		return nil, err
	}
	if m.customReader != nil {
		return m.customReader, nil
	}
	return io.NopCloser(bytes.NewReader([]byte("fake tar data"))), nil
}

func (m *mockClient) execCommand(_ context.Context, containerID, user, cmd string) error {
	m.execCalls = append(m.execCalls, execCall{containerID, user, cmd})
	if err := m.execErrs[cmd]; err != nil {
		return err
	}
	return nil
}

func (m *mockClient) containerLogs(_ context.Context, containerID string, _ container.LogsOptions) (io.ReadCloser, error) {
	m.logsCalls = append(m.logsCalls, containerID)
	if m.logsErr != nil {
		return nil, m.logsErr
	}
	var buf bytes.Buffer
	if m.logsOutput != "" {
		w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
		if _, err := w.Write([]byte(m.logsOutput)); err != nil {
			return nil, err
		}
	}
	return io.NopCloser(strings.NewReader(buf.String())), nil
}

func TestOutputSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "no output"},
		{"whitespace only", "  \n\t ", "no output"},
		{"short", "pg_dump: error\n", "pg_dump: error"},
		{"trailing newline trimmed", "fatal: boom\n\n", "fatal: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outputSummary(tt.in); got != tt.want {
				t.Errorf("outputSummary(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	long := strings.Repeat("x", outputLimit+100)
	got := outputSummary(long)
	if len(got) != outputLimit+3 || !strings.HasPrefix(got, "...") {
		t.Errorf("expected tail of %d bytes with prefix, got %d bytes", outputLimit, len(got))
	}
}

func TestRunHelper_FailureIncludesLogs(t *testing.T) {
	mc := &mockClient{
		imagesPresent: map[string]bool{"registry.example/helper:latest": true},
		waitExitCode:  1,
		logsOutput:    "pg_dumpall: could not connect to server\n",
	}
	argv := `["/usr/bin/pg_dumpall"]`

	err := runHelper(context.Background(), mc, "myapp", "registry.example/helper:latest", argv, helperTestMounts())
	if err == nil {
		t.Fatal("expected error for non-zero helper exit")
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Errorf("expected exit code in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "could not connect to server") {
		t.Errorf("expected helper output in error, got: %v", err)
	}
	if len(mc.logsCalls) != 1 || mc.logsCalls[0] != "helper123456789" {
		t.Errorf("expected one log fetch for the helper container, got %v", mc.logsCalls)
	}
	if len(mc.removeCalls) != 1 {
		t.Errorf("expected helper container to still be removed, got %v", mc.removeCalls)
	}
}

func TestRunHelper_FailureWithoutLogs(t *testing.T) {
	mc := &mockClient{
		imagesPresent: map[string]bool{"registry.example/helper:latest": true},
		waitExitCode:  2,
		logsErr:       errors.New("log driver none"),
	}
	argv := `["/bin/false"]`

	err := runHelper(context.Background(), mc, "myapp", "registry.example/helper:latest", argv, helperTestMounts())
	if err == nil {
		t.Fatal("expected error for non-zero helper exit")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("expected 'no output' placeholder when logs are unavailable, got: %v", err)
	}
}
