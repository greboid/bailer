package main

import (
	"bytes"
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
)

type mockClient struct {
	containers []container.Summary
	listErr    error
	copyCalls  []copyCall
	execCalls  []execCall
	execErrs   map[string]error
	copyErrs   map[string]error
}

type copyCall struct {
	containerID string
	srcPath     string
}

type execCall struct {
	containerID string
	cmd         string
}

func (m *mockClient) listContainers(_ context.Context) ([]container.Summary, error) {
	return m.containers, m.listErr
}

func (m *mockClient) copyFromContainer(_ context.Context, containerID, srcPath string) (io.ReadCloser, error) {
	m.copyCalls = append(m.copyCalls, copyCall{containerID, srcPath})
	if err := m.copyErrs[srcPath]; err != nil {
		return nil, err
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
