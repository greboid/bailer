package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestParseVolumeList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]bool
	}{
		{"empty string", "", nil},
		{"single item", "vol1", map[string]bool{"vol1": true}},
		{"multiple items", "vol1,vol2,vol3", map[string]bool{"vol1": true, "vol2": true, "vol3": true}},
		{"items with spaces", " vol1 , vol2 , vol3 ", map[string]bool{"vol1": true, "vol2": true, "vol3": true}},
		{"trailing comma", "vol1,vol2,", map[string]bool{"vol1": true, "vol2": true}},
		{"leading comma", ",vol1,vol2", map[string]bool{"vol1": true, "vol2": true}},
		{"only commas", ",,,", nil},
		{"spaces only", " , , ", nil},
		{"leading comma", " vol1, ,", map[string]bool{"vol1": true}},
		{"leading comma", ",vol1,    ,vol2", map[string]bool{"vol1": true, "vol2": true}},
		{"wildcard", "*", map[string]bool{"*": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseVolumeList(tt.input)
			if len(result) != len(tt.expected) {
				t.Fatalf("parseVolumeList(%q) returned %d items, want %d", tt.input, len(result), len(tt.expected))
			}
			for k := range tt.expected {
				if !result[k] {
					t.Errorf("parseVolumeList(%q) missing key %q", tt.input, k)
				}
			}
			for k := range result {
				if !tt.expected[k] {
					t.Errorf("parseVolumeList(%q) has unexpected key %q", tt.input, k)
				}
			}
		})
	}
}

func TestContainerName(t *testing.T) {
	tests := []struct {
		name     string
		summary  container.Summary
		expected string
	}{
		{
			"single name with slash",
			container.Summary{ID: "abc123def456", Names: []string{"/mycontainer"}},
			"mycontainer",
		},
		{
			"name without slash",
			container.Summary{ID: "abc123def456", Names: []string{"mycontainer"}},
			"mycontainer",
		},
		{
			"no names falls back to ID",
			container.Summary{ID: "abc123def456789", Names: nil},
			"abc123def456",
		},
		{
			"empty names falls back to ID",
			container.Summary{ID: "abc123def456789", Names: []string{}},
			"abc123def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containerName(tt.summary)
			if result != tt.expected {
				t.Errorf("containerName() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestMountIdentifier(t *testing.T) {
	tests := []struct {
		name     string
		mount    container.MountPoint
		expected string
	}{
		{
			"volume mount uses name",
			container.MountPoint{Type: mount.TypeVolume, Name: "mydata", Destination: "/data"},
			"mydata",
		},
		{
			"bind mount uses destination",
			container.MountPoint{Type: mount.TypeBind, Name: "", Destination: "/host/path"},
			"/host/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mountIdentifier(tt.mount)
			if result != tt.expected {
				t.Errorf("mountIdentifier() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFilterVolumes(t *testing.T) {
	volMount := func(name, dest string) container.MountPoint {
		return container.MountPoint{Type: mount.TypeVolume, Name: name, Destination: dest, RW: true}
	}
	bindMount := func(dest string) container.MountPoint {
		return container.MountPoint{Type: mount.TypeBind, Destination: dest, RW: true}
	}
	readOnlyMount := func(name string) container.MountPoint {
		return container.MountPoint{Type: mount.TypeVolume, Name: name, Destination: "/data", RW: false}
	}

	tests := []struct {
		name     string
		mode     string
		mounts   []container.MountPoint
		labels   map[string]string
		expected int
	}{
		{"exclude no exclusions", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			nil, 2},
		{"exclude specific volume", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			map[string]string{labelExclude: "/data"}, 1},
		{"exclude wildcard", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			map[string]string{labelExclude: "*"}, 0},
		{"include no inclusions label", "include",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			map[string]string{labelInclude: ""}, 0},
		{"include nil labels returns none", "include",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			nil, 0},
		{"include specific volume", "include",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			map[string]string{labelInclude: "/data"}, 1},
		{"include wildcard", "include",
			[]container.MountPoint{volMount("vol1", "/data"), volMount("vol2", "/config")},
			map[string]string{labelInclude: "*"}, 2},
		{"filters out read-only", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), readOnlyMount("vol2")},
			nil, 1},
		{"exclude bind mount by destination", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), bindMount("/host/path")},
			map[string]string{labelExclude: "/host/path"}, 1},
		{"include bind mount by destination", "include",
			[]container.MountPoint{volMount("vol1", "/data"), bindMount("/host/path")},
			map[string]string{labelInclude: "/host/path"}, 1},
		{"excludes docker socket", "exclude",
			[]container.MountPoint{volMount("vol1", "/data"), bindMount("/var/run/docker.sock")},
			nil, 1},
		{"excludes docker socket in include mode", "include",
			[]container.MountPoint{volMount("vol1", "/data"), bindMount("/var/run/docker.sock")},
			map[string]string{labelInclude: "*"}, 1},
		{"empty mounts", "exclude", nil, nil, 0},
		{"exclude multiple volumes", "exclude",
			[]container.MountPoint{volMount("vol1", "/a"), volMount("vol2", "/b"), volMount("vol3", "/c")},
			map[string]string{labelExclude: "/a,/c"}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterVolumes(tt.mode, tt.mounts, tt.labels)
			if len(result) != tt.expected {
				t.Errorf("filterVolumes() returned %d volumes, want %d", len(result), tt.expected)
			}
		})
	}
}

func TestRun_ListError(t *testing.T) {
	mc := &mockClient{listErr: errors.New("docker unavailable")}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err == nil {
		t.Fatal("expected error when listing containers fails")
	}
	if !strings.Contains(err.Error(), "listing containers") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_NoContainers(t *testing.T) {
	mc := &mockClient{containers: nil}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 0 || len(mc.copyCalls) != 0 {
		t.Error("expected no calls when no containers")
	}
}

func TestRun_BackupsStoppedContainer(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:     "stopped123456789",
				State:  "stopped",
				Names:  []string{"/stopped"},
				Labels: map[string]string{},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 backup for stopped container, got %d", len(mc.copyCalls))
	}
}

func TestRun_StoppedContainerSkipsExecCommands(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "stopped123456789",
				State: "stopped",
				Names: []string{"/stopped"},
				Labels: map[string]string{
					labelPreBackupCmd:  "dump",
					labelPostBackupCmd: "cleanup",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 0 {
		t.Errorf("expected no exec calls for stopped container, got %d", len(mc.execCalls))
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 backup for stopped container, got %d", len(mc.copyCalls))
	}
}

func TestRun_BackupsRunningContainer(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:     "running123456789",
				State:  "running",
				Names:  []string{"/myapp"},
				Labels: map[string]string{},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
					{Type: mount.TypeVolume, Name: "vol2", Destination: "/config", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 2 {
		t.Errorf("expected 2 copy calls, got %d", len(mc.copyCalls))
	}
}

func TestRun_IncludeModeFiltersContainers(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:     "skipped123456789",
				State:  "running",
				Names:  []string{"/skipped"},
				Labels: map[string]string{},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
			{
				ID:     "included123456789",
				State:  "running",
				Names:  []string{"/included"},
				Labels: map[string]string{labelInclude: "/data"},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "include")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 copy call (only included container), got %d", len(mc.copyCalls))
	}
}

func TestRun_PreBackupCommand(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "running123456789",
				State: "running",
				Names: []string{"/myapp"},
				Labels: map[string]string{
					labelPreBackupCmd: "pg_dump",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 1 || mc.execCalls[0].cmd != "pg_dump" {
		t.Errorf("expected pre-backup exec call, got %v", mc.execCalls)
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 copy call after pre-backup, got %d", len(mc.copyCalls))
	}
}

func TestRun_PreBackupFailsSkipsContainer(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "running123456789",
				State: "running",
				Names: []string{"/myapp"},
				Labels: map[string]string{
					labelPreBackupCmd: "fail_cmd",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
		execErrs: map[string]error{"fail_cmd": errors.New("command failed")},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 0 {
		t.Error("expected no copy calls when pre-backup fails")
	}
}

func TestRun_PostBackupCommand(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "running123456789",
				State: "running",
				Names: []string{"/myapp"},
				Labels: map[string]string{
					labelPostBackupCmd: "cleanup",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 1 || mc.execCalls[0].cmd != "cleanup" {
		t.Errorf("expected post-backup exec call, got %v", mc.execCalls)
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 copy call before post-backup, got %d", len(mc.copyCalls))
	}
}

func TestRun_PostBackupFailureDoesNotBlock(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "running123456789",
				State: "running",
				Names: []string{"/myapp"},
				Labels: map[string]string{
					labelPostBackupCmd: "fail_cleanup",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
		execErrs: map[string]error{"fail_cleanup": errors.New("cleanup failed")},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.copyCalls) != 1 {
		t.Error("expected backups to complete even when post-backup fails")
	}
}

func TestRun_BothPreAndPostBackup(t *testing.T) {
	mc := &mockClient{
		containers: []container.Summary{
			{
				ID:    "running123456789",
				State: "running",
				Names: []string{"/myapp"},
				Labels: map[string]string{
					labelPreBackupCmd:  "dump",
					labelPostBackupCmd: "cleanup",
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "vol1", Destination: "/data", RW: true},
				},
			},
		},
	}
	err := run(context.Background(), mc, t.TempDir(), "exclude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.execCalls) != 2 {
		t.Fatalf("expected 2 exec calls, got %d", len(mc.execCalls))
	}
	if mc.execCalls[0].cmd != "dump" {
		t.Errorf("expected first exec to be pre-backup, got %q", mc.execCalls[0].cmd)
	}
	if mc.execCalls[1].cmd != "cleanup" {
		t.Errorf("expected second exec to be post-backup, got %q", mc.execCalls[1].cmd)
	}
	if len(mc.copyCalls) != 1 {
		t.Errorf("expected 1 copy call between pre and post, got %d", len(mc.copyCalls))
	}
}

func TestBackupVolume(t *testing.T) {
	mc := &mockClient{}
	tmpDir := t.TempDir()
	vol := container.MountPoint{
		Type:        mount.TypeVolume,
		Name:        "testvol",
		Destination: "/data",
		RW:          true,
	}

	err := backupVolume(context.Background(), mc, "container123456", "testcontainer", tmpDir, vol)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containerDir := filepath.Join(tmpDir, "testcontainer")
	entries, err := os.ReadDir(containerDir)
	if err != nil {
		t.Fatalf("failed to read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 backup file, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "testvol_") || !strings.HasSuffix(entries[0].Name(), ".tar.gz") {
		t.Errorf("unexpected filename: %s", entries[0].Name())
	}
	if len(mc.copyCalls) != 1 || mc.copyCalls[0].srcPath != "/data" {
		t.Errorf("expected copy call for /data, got %v", mc.copyCalls)
	}
}

func TestBackupVolume_CopyError(t *testing.T) {
	mc := &mockClient{
		copyErrs: map[string]error{"/data": errors.New("copy failed")},
	}
	vol := container.MountPoint{
		Type:        mount.TypeVolume,
		Name:        "testvol",
		Destination: "/data",
		RW:          true,
	}

	err := backupVolume(context.Background(), mc, "container123456", "testcontainer", t.TempDir(), vol)
	if err == nil {
		t.Fatal("expected error when copy fails")
	}
	if !strings.Contains(err.Error(), "copying from container") {
		t.Errorf("unexpected error: %v", err)
	}
}
