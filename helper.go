package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// parseArgvLabel parses a helper command label, which must be a JSON array of
// strings used directly as argv (helper images have no shell).
func parseArgvLabel(s string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(s), &argv); err != nil {
		return nil, fmt.Errorf("parsing command as JSON array: %w", err)
	}
	if len(argv) == 0 {
		return nil, errors.New("command is an empty array")
	}
	if slices.Contains(argv, "") {
		return nil, errors.New("command contains an empty argument")
	}
	return argv, nil
}

// runHelper runs argv in an ephemeral container using the given image, with the
// target container's volume and bind mounts attached. The helper container is
// always removed afterwards.
func runHelper(ctx context.Context, cli dockerAPI, targetName, image, argv string, mounts []container.MountPoint) error {
	entrypoint, err := parseArgvLabel(argv)
	if err != nil {
		return fmt.Errorf("invalid helper command for %s: %w", targetName, err)
	}

	if err := ensureHelperImage(ctx, cli, image); err != nil {
		return err
	}

	created, err := cli.containerCreate(ctx, container.Config{
		Image:      image,
		Entrypoint: entrypoint,
		User:       "0",
	}, container.HostConfig{
		Mounts: helperMounts(mounts),
	})
	if err != nil {
		return fmt.Errorf("creating helper container for %s: %w", targetName, err)
	}

	defer func() {
		if rmErr := cli.containerRemove(ctx, created.ID, container.RemoveOptions{Force: true}); rmErr != nil && !client.IsErrNotFound(rmErr) {
			slog.Warn("Failed to remove helper container", "container", targetName, "id", created.ID[:12], "error", rmErr)
		}
	}()

	if err := cli.containerStart(ctx, created.ID); err != nil {
		return fmt.Errorf("starting helper container for %s: %w", targetName, err)
	}

	statusCh, errCh := cli.containerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var exitCode int64
	select {
	case err := <-errCh:
		return fmt.Errorf("waiting for helper container of %s: %w", targetName, err)
	case resp := <-statusCh:
		if resp.Error != nil {
			return fmt.Errorf("helper container for %s failed: %s", targetName, resp.Error.Message)
		}
		exitCode = resp.StatusCode
	}

	if exitCode != 0 {
		return fmt.Errorf("helper container for %s exited with code %d", targetName, exitCode)
	}
	return nil
}

func ensureHelperImage(ctx context.Context, cli dockerAPI, image string) error {
	if _, err := cli.imageInspect(ctx, image); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspecting helper image %s: %w", image, err)
	}

	reader, err := cli.imagePull(ctx, image)
	if err != nil {
		return fmt.Errorf("pulling helper image %s: %w", image, err)
	}
	defer reader.Close()

	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("reading pull progress for helper image %s: %w", image, err)
	}
	return nil
}

func helperMounts(mounts []container.MountPoint) []mount.Mount {
	var result []mount.Mount
	for _, m := range mounts {
		if m.Type != mount.TypeVolume && m.Type != mount.TypeBind {
			continue
		}
		if m.Destination == "/var/run/docker.sock" {
			continue
		}
		// For named volumes MountPoint.Source is the host storage path
		// (/var/lib/docker/volumes/<name>/_data); mount.Mount.Source must be
		// the volume name instead.
		source := m.Source
		if m.Type == mount.TypeVolume && m.Name != "" {
			source = m.Name
		}
		result = append(result, mount.Mount{
			Type:   m.Type,
			Source: source,
			Target: m.Destination,
		})
	}
	return result
}
