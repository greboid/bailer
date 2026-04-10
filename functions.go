package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func runDaemon(ctx context.Context, cli dockerAPI, backupDir, mode string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("Running in mode", "interval", interval)

	for {
		slog.Info("Starting scheduled backup")
		if err := run(ctx, cli, backupDir, mode); err != nil {
			slog.Error("Scheduled backup failed", "error", err)
		}

		select {
		case <-ctx.Done():
			slog.Info("Shutting down")
			return
		case <-ticker.C:
		}
	}
}

func run(ctx context.Context, cli dockerAPI, backupDir, mode string) error {
	containers, err := cli.listContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	for _, c := range containers {
		name := containerName(c)
		log := slog.With("container", name, "id", c.ID[:12])

		volumes := filterVolumes(mode, c.Mounts, c.Labels)
		if len(volumes) == 0 {
			log.Debug("No volumes to backup, skipping")
			continue
		}

		log.Info("Processing container", "volumes", len(volumes), "running", c.State == "running")

		if c.State == "running" {
			if cmd, ok := c.Labels[labelPreBackupCmd]; ok {
				log.Info("Running pre-backup command", "cmd", cmd)
				if err := cli.execCommand(ctx, c.ID, cmd); err != nil {
					log.Error("Pre-backup command failed, skipping container", "error", err)
					continue
				}
			}
		}

		for _, vol := range volumes {
			volLog := log.With("volume", mountIdentifier(vol), "type", vol.Type, "destination", vol.Destination)
			volLog.Info("Backing up volume")
			if err := backupVolume(ctx, cli, c.ID, name, backupDir, vol); err != nil {
				volLog.Error("Failed to backup volume", "error", err)
			}
		}

		if c.State == "running" {
			if cmd, ok := c.Labels[labelPostBackupCmd]; ok {
				log.Info("Running post-backup command", "cmd", cmd)
				if err := cli.execCommand(ctx, c.ID, cmd); err != nil {
					log.Error("Post-backup command failed", "error", err)
				}
			}
		}
	}

	return nil
}

func dryRunMode(ctx context.Context, cli dockerAPI, mode string) error {
	containers, err := cli.listContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	fmt.Printf("%-30s %-20s %-10s %-30s %s\n", "CONTAINER", "VOLUME", "TYPE", "DESTINATION", "BACKUP")
	fmt.Printf("%s\n", strings.Repeat("-", 113))

	for _, c := range containers {
		name := containerName(c)
		if c.State != "running" {
			fmt.Printf("%-30s %-20s %-10s %-30s %s\n", name, "-", "-", "-", "no (not running)")
			continue
		}

		if len(c.Mounts) == 0 {
			fmt.Printf("%-30s %-20s %-10s %-30s %s\n", name, "-", "-", "-", "no (no mounts)")
			continue
		}

		filtered := filterVolumes(mode, c.Mounts, c.Labels)
		filteredSet := make(map[string]bool, len(filtered))
		for _, v := range filtered {
			filteredSet[mountIdentifier(v)] = true
		}

		for _, m := range c.Mounts {
			if m.Type != mount.TypeVolume && m.Type != mount.TypeBind {
				continue
			}
			id := mountIdentifier(m)
			willBackup := filteredSet[id]
			backupStr := "yes"
			if !willBackup {
				backupStr = "no (filtered)"
			}
			if !m.RW {
				backupStr = "no (read-only)"
			}
			fmt.Printf("%-30s %-20s %-10s %-30s %s\n", name, id, m.Type, m.Destination, backupStr)
		}
	}

	return nil
}

func filterVolumes(mode string, mounts []container.MountPoint, labels map[string]string) []container.MountPoint {
	var names map[string]bool
	switch mode {
	case "include":
		if _, ok := labels[labelInclude]; !ok {
			return nil
		}
		names = parseVolumeList(labels[labelInclude])
	case "exclude":
		names = parseVolumeList(labels[labelExclude])
	}

	var result []container.MountPoint
	for _, m := range mounts {
		if (m.Type != mount.TypeVolume && m.Type != mount.TypeBind) || !m.RW {
			continue
		}
		if m.Destination == "/var/run/docker.sock" {
			continue
		}
		if mode == "exclude" && len(names) > 0 && mountMatches(names, m) {
			continue
		}
		if mode == "include" && !mountMatches(names, m) {
			continue
		}
		result = append(result, m)
	}
	return result
}

func mountIdentifier(m container.MountPoint) string {
	if m.Type == mount.TypeBind {
		return m.Destination
	}
	return m.Name
}

func mountMatches(names map[string]bool, m container.MountPoint) bool {
	if names["*"] {
		return true
	}
	return names[m.Destination]
}

func parseVolumeList(s string) map[string]bool {
	if s == "" {
		return nil
	}
	result := make(map[string]bool)
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			result[name] = true
		}
	}
	return result
}

func backupVolume(ctx context.Context, cli dockerAPI, containerID, containerName, backupDir string, vol container.MountPoint) error {
	timestamp := time.Now().Format("20060102-150405")
	id := mountIdentifier(vol)
	sanitized := strings.ReplaceAll(strings.Trim(id, "/"), "/", "_")
	backupName := fmt.Sprintf("%s_%s.tar.gz", sanitized, timestamp)

	containerDir := filepath.Join(backupDir, containerName)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("creating backup directory: %w", err)
	}
	backupPath := filepath.Join(containerDir, backupName)

	reader, err := cli.copyFromContainer(ctx, containerID, vol.Destination)
	if err != nil {
		return fmt.Errorf("copying from container: %w", err)
	}
	defer reader.Close()

	outFile, err := os.Create(backupPath)
	if err != nil {
		return fmt.Errorf("creating backup file: %w", err)
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	if _, err := io.Copy(gzWriter, reader); err != nil {
		return fmt.Errorf("writing backup file: %w", err)
	}

	return nil
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID[:12]
}
