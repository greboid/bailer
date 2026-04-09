package main

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type dockerAPI interface {
	listContainers(ctx context.Context) ([]container.Summary, error)
	copyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error)
	execCommand(ctx context.Context, containerID, cmd string) error
}

type dockerClient struct {
	*client.Client
}

func (c dockerClient) listContainers(ctx context.Context) ([]container.Summary, error) {
	return c.ContainerList(ctx, container.ListOptions{})
}

func (c dockerClient) copyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error) {
	r, _, err := c.CopyFromContainer(ctx, containerID, srcPath)
	return r, err
}

func (c dockerClient) execCommand(ctx context.Context, containerID, cmd string) error {
	execCreate, err := c.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{"sh", "-c", cmd},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("creating exec: %w", err)
	}

	hijacked, err := c.ContainerExecAttach(ctx, execCreate.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("attaching exec: %w", err)
	}
	defer hijacked.Close()

	if err := c.ContainerExecStart(ctx, execCreate.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("starting exec: %w", err)
	}

	_, _ = io.ReadAll(hijacked.Reader)

	inspect, err := c.ContainerExecInspect(ctx, execCreate.ID)
	if err != nil {
		return fmt.Errorf("inspecting exec: %w", err)
	}

	if inspect.ExitCode != 0 {
		return fmt.Errorf("exec exited with code %d", inspect.ExitCode)
	}

	return nil
}
