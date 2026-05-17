package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const labelKey = "managed-by"
const labelVal = "sockt-pod-mock"

type DockerManager struct {
	cli      *client.Client
	image    string
	network  string // docker network to attach containers to (enables DNS routing)
	nextPort atomic.Int64
}

func NewDockerManager() (*DockerManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	image := os.Getenv("POD_MOCK_RUNTIME_IMAGE")
	if image == "" {
		image = "sockt-runtime"
	}

	portBase := 18000
	if v := os.Getenv("POD_MOCK_PORT_BASE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			portBase = n
		}
	}

	dm := &DockerManager{cli: cli, image: image, network: os.Getenv("POD_MOCK_NETWORK")}
	dm.nextPort.Store(int64(portBase))
	return dm, nil
}

func (dm *DockerManager) Network() string { return dm.network }

func (dm *DockerManager) allocPort() int {
	return int(dm.nextPort.Add(1) - 1)
}

func (dm *DockerManager) CreateContainer(ctx context.Context, name string, env map[string]string) (string, int, string, error) {
	port := dm.allocPort()
	containerName := "sockt-pod-" + name

	envSlice := make([]string, 0, len(env)+1)
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	envSlice = append(envSlice, "LISTEN_ADDR=:8443")

	containerPort := nat.Port("8443/tcp")
	hostBinding := nat.PortBinding{HostIP: "0.0.0.0", HostPort: strconv.Itoa(port)}

	resp, err := dm.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        dm.image,
			Env:          envSlice,
			Labels:       map[string]string{labelKey: labelVal},
			ExposedPorts: nat.PortSet{containerPort: struct{}{}},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				containerPort: []nat.PortBinding{hostBinding},
			},
			ExtraHosts: []string{"host.docker.internal:host-gateway"},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return "", 0, "", fmt.Errorf("container create: %w", err)
	}

	if err := dm.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = dm.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", 0, "", fmt.Errorf("container start: %w", err)
	}

	if dm.network != "" {
		if err := dm.cli.NetworkConnect(ctx, dm.network, resp.ID, &network.EndpointSettings{}); err != nil {
			slog.Warn("network_connect", "container", containerName, "network", dm.network, "err", err)
		}
	}

	// Detect immediate crashes so callers get a fast error instead of a 120s health-check timeout.
	time.Sleep(500 * time.Millisecond)
	inspect, err := dm.cli.ContainerInspect(ctx, resp.ID)
	if err == nil && !inspect.State.Running {
		logs := dm.tailLogs(ctx, resp.ID)
		_ = dm.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", 0, "", fmt.Errorf("container exited immediately: %s", logs)
	}

	return resp.ID, port, containerName, nil
}

func (dm *DockerManager) tailLogs(ctx context.Context, containerID string) string {
	rc, err := dm.cli.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return "(no logs)"
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 4096))
	if len(b) == 0 {
		return "(empty)"
	}
	return string(b)
}

func (dm *DockerManager) StopContainer(ctx context.Context, containerID string) error {
	return dm.cli.ContainerStop(ctx, containerID, container.StopOptions{})
}

func (dm *DockerManager) StartContainer(ctx context.Context, containerID string) error {
	return dm.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (dm *DockerManager) RemoveContainer(ctx context.Context, containerID string) error {
	return dm.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// RecreateContainer stops and removes the existing container, then creates a new one with the
// updated env while preserving the same name, port, and network attachment. This mirrors RunPod's
// behavior when pod env is updated via API — the pod restarts with the new environment.
func (dm *DockerManager) RecreateContainer(ctx context.Context, oldContainerID, containerName string, hostPort int, env map[string]string) (string, error) {
	_ = dm.cli.ContainerStop(ctx, oldContainerID, container.StopOptions{})
	_ = dm.cli.ContainerRemove(ctx, oldContainerID, container.RemoveOptions{Force: true})

	envSlice := make([]string, 0, len(env)+1)
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	envSlice = append(envSlice, "LISTEN_ADDR=:8443")

	containerPort := nat.Port("8443/tcp")
	hostBinding := nat.PortBinding{HostIP: "0.0.0.0", HostPort: strconv.Itoa(hostPort)}

	resp, err := dm.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        dm.image,
			Env:          envSlice,
			Labels:       map[string]string{labelKey: labelVal},
			ExposedPorts: nat.PortSet{containerPort: struct{}{}},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				containerPort: []nat.PortBinding{hostBinding},
			},
			ExtraHosts: []string{"host.docker.internal:host-gateway"},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return "", fmt.Errorf("container recreate: %w", err)
	}

	if err := dm.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = dm.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("container restart: %w", err)
	}

	if dm.network != "" {
		if err := dm.cli.NetworkConnect(ctx, dm.network, resp.ID, &network.EndpointSettings{}); err != nil {
			slog.Warn("network_connect", "container", containerName, "network", dm.network, "err", err)
		}
	}

	return resp.ID, nil
}

func (dm *DockerManager) Cleanup(ctx context.Context) {
	f := filters.NewArgs(filters.Arg("label", labelKey+"="+labelVal))
	containers, err := dm.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		slog.Error("cleanup: list", "err", err)
		return
	}
	for _, c := range containers {
		slog.Info("cleanup: removing", "id", c.ID[:12])
		_ = dm.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}
