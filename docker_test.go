package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
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
	inspectCalls  []string
	pullCalls     []string
	createCalls   []createCall
	startCalls    []string
	waitCalls     []waitCall
	removeCalls   []string
	customReader  io.ReadCloser
}

type copyCall struct {
	containerID string
	srcPath     string
}

type execCall struct {
	containerID string
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

func (m *mockClient) execCommand(_ context.Context, containerID, cmd string) error {
	m.execCalls = append(m.execCalls, execCall{containerID, cmd})
	if err := m.execErrs[cmd]; err != nil {
		return err
	}
	return nil
}

func TestExecOutputSummary(t *testing.T) {
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
			if got := execOutputSummary(tt.in); got != tt.want {
				t.Errorf("execOutputSummary(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	long := strings.Repeat("x", execOutputLimit+100)
	got := execOutputSummary(long)
	if len(got) != execOutputLimit+3 || !strings.HasPrefix(got, "...") {
		t.Errorf("expected tail of %d bytes with prefix, got %d bytes", execOutputLimit, len(got))
	}
}
