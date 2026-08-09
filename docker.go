package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type dockerClient struct {
	api       client.APIClient
	mu        sync.Mutex
	envCache  map[string]map[string]string
	binCache  map[string]map[string]bool
	nameCache map[string][]string
}

func newDockerClient(ctx context.Context) (*dockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerClient{
		api:       cli,
		envCache:  map[string]map[string]string{},
		binCache:  map[string]map[string]bool{},
		nameCache: map[string][]string{},
	}, nil
}

func (dc *dockerClient) listContainers(ctx context.Context) ([]container.Summary, error) {
	res, err := dc.api.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}

func (dc *dockerClient) containerImageNames(ctx context.Context, containerID string) ([]string, error) {
	dc.mu.Lock()
	if names, ok := dc.nameCache[containerID]; ok {
		dc.mu.Unlock()
		return names, nil
	}
	dc.mu.Unlock()

	inspect, err := dc.api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	imgInspect, err := dc.api.ImageInspect(ctx, inspect.Container.Config.Image)
	if err != nil {
		return nil, err
	}
	names := imageNamesFromTags(imgInspect.RepoTags)

	dc.mu.Lock()
	dc.nameCache[containerID] = names
	dc.mu.Unlock()
	return names, nil
}

func (dc *dockerClient) containerEnv(ctx context.Context, containerID string) (map[string]string, error) {
	dc.mu.Lock()
	if env, ok := dc.envCache[containerID]; ok {
		dc.mu.Unlock()
		return env, nil
	}
	dc.mu.Unlock()

	out, err := dc.execCollect(ctx, containerID, []string{"env"}, nil)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) == 2 {
			env[kv[0]] = kv[1]
		}
	}

	dc.mu.Lock()
	dc.envCache[containerID] = env
	dc.mu.Unlock()
	return env, nil
}

func (dc *dockerClient) hasBinary(ctx context.Context, containerID, binary string) (bool, error) {
	dc.mu.Lock()
	if m, ok := dc.binCache[containerID]; ok {
		if exists, ok := m[binary]; ok {
			dc.mu.Unlock()
			return exists, nil
		}
	}
	dc.mu.Unlock()

	execID, attach, err := dc.startExec(ctx, containerID, []string{"which", binary}, nil)
	if err != nil {
		return false, err
	}
	defer attach.Close()
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.Discard, &stderr, attach.Reader); err != nil {
		return false, err
	}
	info, err := dc.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return false, err
	}
	exists := info.ExitCode == 0

	dc.mu.Lock()
	if dc.binCache[containerID] == nil {
		dc.binCache[containerID] = map[string]bool{}
	}
	dc.binCache[containerID][binary] = exists
	dc.mu.Unlock()
	return exists, nil
}

func (dc *dockerClient) execCollect(ctx context.Context, containerID string, cmd, env []string) ([]byte, error) {
	execID, attach, err := dc.startExec(ctx, containerID, cmd, env)
	if err != nil {
		return nil, err
	}
	defer attach.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		return nil, err
	}
	info, err := dc.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return nil, err
	}
	if info.ExitCode != 0 {
		return nil, fmt.Errorf("命令执行失败 (exit %d): %s", info.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (dc *dockerClient) startExec(ctx context.Context, containerID string, cmd, env []string) (string, client.HijackedResponse, error) {
	resp, err := dc.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          false,
	})
	if err != nil {
		return "", client.HijackedResponse{}, err
	}
	attach, err := dc.api.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", client.HijackedResponse{}, err
	}
	return resp.ID, attach.HijackedResponse, nil
}
