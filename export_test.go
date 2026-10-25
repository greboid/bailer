package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestValidateExportPath(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		expectErr string
	}{
		{"valid path", "/data", ""},
		{"nested path", "/var/lib/data", ""},
		{"empty path", "", "must not be empty"},
		{"relative path", "data", "must be absolute"},
		{"trailing slash is not canonical", "/data/", "not canonical"},
		{"double slash is not canonical", "//data", "not canonical"},
		{"dot component is not canonical", "/data/.", "not canonical"},
		{"dotdot traversal is not canonical", "/data/../etc", "not canonical"},
		{"container root refused", "/", "refusing to export the container root filesystem"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExportPath(tt.path)
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

func TestExportVolume_Success(t *testing.T) {
	mc := &mockClient{}
	var dst strings.Builder

	if err := exportVolume(context.Background(), mc, "abc123", "/data", &dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 1 {
		t.Fatalf("expected 1 copy call, got %d", len(mc.copyCalls))
	}
	if mc.copyCalls[0].containerID != "abc123" || mc.copyCalls[0].srcPath != "/data" {
		t.Errorf("unexpected copy call: %+v", mc.copyCalls[0])
	}
	// The output must be exactly the tar stream, with no logs or decoration.
	if dst.String() != "fake tar data" {
		t.Errorf("expected pure tar stream, got %q", dst.String())
	}
}

func TestExportVolume_EmptyContainerID(t *testing.T) {
	mc := &mockClient{}
	if err := exportVolume(context.Background(), mc, "", "/data", io.Discard); err == nil {
		t.Fatal("expected error for empty container id")
	}
	if len(mc.copyCalls) != 0 {
		t.Error("expected no copy calls for empty container id")
	}
}

func TestExportVolume_InvalidPath(t *testing.T) {
	for _, path := range []string{"", "data", "/data/", "/", "/data/../etc"} {
		mc := &mockClient{}
		if err := exportVolume(context.Background(), mc, "abc123", path, io.Discard); err == nil {
			t.Errorf("expected error for path %q", path)
		}
		if len(mc.copyCalls) != 0 {
			t.Errorf("expected no copy calls for path %q", path)
		}
	}
}

func TestExportVolume_CopyError(t *testing.T) {
	mc := &mockClient{copyErrs: map[string]error{"/data": errors.New("copy failed")}}
	if err := exportVolume(context.Background(), mc, "abc123", "/data", io.Discard); err == nil {
		t.Fatal("expected error when copy fails")
	} else if !strings.Contains(err.Error(), "copying from container") {
		t.Errorf("unexpected error: %v", err)
	}
}

// failingWriter fails after a configurable number of bytes so that a write
// error part-way through the stream can be simulated.
type failingWriter struct {
	failAfter int
	written   int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfter {
		return 0, errors.New("simulated write failure")
	}
	w.written += len(p)
	return len(p), nil
}

func TestExportVolume_WriteError(t *testing.T) {
	mc := &mockClient{}
	dst := &failingWriter{failAfter: 4}

	err := exportVolume(context.Background(), mc, "abc123", "/data", dst)
	if err == nil {
		t.Fatal("expected error when writing fails")
	}
	if !strings.Contains(err.Error(), "streaming tar archive") {
		t.Errorf("unexpected error: %v", err)
	}
}

// erroringReader returns data then an error, simulating a stream that breaks
// part-way through the transfer.
type erroringReader struct {
	data []byte
	pos  int
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errors.New("simulated read failure")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *erroringReader) Close() error { return nil }

func TestExportVolume_ReadError(t *testing.T) {
	mc := &mockClient{
		customReader: &erroringReader{data: []byte("partial")},
	}

	err := exportVolume(context.Background(), mc, "abc123", "/data", io.Discard)
	if err == nil {
		t.Fatal("expected error when reading fails")
	}
	if !strings.Contains(err.Error(), "streaming tar archive") {
		t.Errorf("unexpected error: %v", err)
	}
}

// closeErrReader yields data successfully but fails on close, verifying that
// closing the Docker response stream is checked.
type closeErrReader struct {
	io.Reader
	closed bool
}

func (r *closeErrReader) Close() error {
	r.closed = true
	return errors.New("simulated close failure")
}

func TestExportVolume_CloseError(t *testing.T) {
	reader := &closeErrReader{Reader: strings.NewReader("fake tar data")}
	mc := &mockClient{customReader: reader}

	err := exportVolume(context.Background(), mc, "abc123", "/data", io.Discard)
	if err == nil {
		t.Fatal("expected error when closing the stream fails")
	}
	if !strings.Contains(err.Error(), "closing docker response stream") {
		t.Errorf("unexpected error: %v", err)
	}
	if !reader.closed {
		t.Error("expected reader to be closed even on close failure")
	}
}

// closeTrackingReader verifies the response stream is closed after a
// mid-stream read error as well.
type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestExportVolume_StreamClosedAfterReadError(t *testing.T) {
	reader := &closeTrackingReader{Reader: &erroringReader{data: []byte("partial")}}
	mc := &mockClient{customReader: reader}

	_ = exportVolume(context.Background(), mc, "abc123", "/data", io.Discard)
	if !reader.closed {
		t.Error("expected reader to be closed after read failure")
	}
}

func TestRunExportCommand_ArgumentValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		expectCode int
	}{
		{"missing container id", []string{"--path", "/data"}, 2},
		{"missing path", []string{"--container-id", "abc123"}, 2},
		{"no arguments", nil, 2},
		{"unknown flag", []string{"--bogus"}, 2},
		{"invalid path rejected before docker", []string{"--container-id", "abc123", "--path", "data"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := runExportCommand(tt.args); code != tt.expectCode {
				t.Errorf("runExportCommand(%v) = %d, want %d", tt.args, code, tt.expectCode)
			}
		})
	}
}
