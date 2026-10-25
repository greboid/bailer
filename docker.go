package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// execOutputLimit caps how much captured exec output is included in an error.
const execOutputLimit = 1024

type dockerAPI interface {
	listContainers(ctx context.Context) ([]container.Summary, error)
	copyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error)
	execCommand(ctx context.Context, containerID, cmd string) error
	imageInspect(ctx context.Context, image string) (image.InspectResponse, error)
	imagePull(ctx context.Context, image string) (io.ReadCloser, error)
	containerCreate(ctx context.Context, config container.Config, hostConfig container.HostConfig) (container.CreateResponse, error)
	containerStart(ctx context.Context, containerID string) error
	containerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	containerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

type dockerClient struct {
	*client.Client
}

func (c dockerClient) listContainers(ctx context.Context) ([]container.Summary, error) {
	return c.ContainerList(ctx, container.ListOptions{All: true})
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

	// The exec stream is multiplexed (no TTY): demux stdout and stderr into
	// one buffer so failures can carry the command's output.
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, hijacked.Reader); err != nil {
		return fmt.Errorf("reading exec output: %w", err)
	}

	inspect, err := c.ContainerExecInspect(ctx, execCreate.ID)
	if err != nil {
		return fmt.Errorf("inspecting exec: %w", err)
	}

	if inspect.ExitCode != 0 {
		return fmt.Errorf("exec exited with code %d: %s", inspect.ExitCode, execOutputSummary(out.String()))
	}

	return nil
}

// execOutputSummary returns the tail of an exec's output for inclusion in an
// error message; failures print their reason at the end.
func execOutputSummary(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "no output"
	}
	if len(out) > execOutputLimit {
		out = "..." + out[len(out)-execOutputLimit:]
	}
	return out
}

func (c dockerClient) imageInspect(ctx context.Context, ref string) (image.InspectResponse, error) {
	return c.ImageInspect(ctx, ref)
}

func (c dockerClient) imagePull(ctx context.Context, ref string) (io.ReadCloser, error) {
	return c.ImagePull(ctx, ref, image.PullOptions{})
}

func (c dockerClient) containerCreate(ctx context.Context, config container.Config, hostConfig container.HostConfig) (container.CreateResponse, error) {
	return c.ContainerCreate(ctx, &config, &hostConfig, nil, nil, "")
}

func (c dockerClient) containerStart(ctx context.Context, containerID string) error {
	return c.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (c dockerClient) containerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return c.ContainerWait(ctx, containerID, condition)
}

func (c dockerClient) containerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	return c.ContainerRemove(ctx, containerID, options)
}
