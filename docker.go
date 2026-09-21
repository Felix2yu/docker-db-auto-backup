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

// labelBackupProvider 允许在源容器上直接声明备份 provider，优先级高于镜像名识别。
const labelBackupProvider = "backup.provider"

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

// containerImageNames 按兜底链解析容器镜像名（C6）：
// RepoTags → RepoDigests → Config.Image → 容器 labels。
// 任一环节命中都会记录来源，避免"识别不到就静默跳过"。
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

	var names []string
	source := ""

	if inspect.Container.Config != nil {
		imageRef := inspect.Container.Config.Image
		imgInspect, err := dc.api.ImageInspect(ctx, imageRef)
		if err != nil {
			logWarn("镜像信息读取失败，将尝试其他方式识别", "container", containerID, "image", imageRef, "error", err)
		} else {
			names = imageNamesFromTags(imgInspect.RepoTags)
			source = "RepoTags"
			if len(names) == 0 && len(imgInspect.RepoDigests) > 0 {
				names = imageNamesFromDigests(imgInspect.RepoDigests)
				source = "RepoDigests"
			}
		}
		if len(names) == 0 && imageRef != "" && !strings.HasPrefix(imageRef, "sha256:") {
			if n := imageNameFromTag(imageRef); n != "" {
				names = []string{n}
				source = "Config.Image"
			}
		}
	}

	if len(names) == 0 && inspect.Container.Config != nil {
		for _, key := range []string{"com.docker.compose.image", "org.opencontainers.image.ref.name"} {
			if v := inspect.Container.Config.Labels[key]; v != "" {
				if n := imageNameFromTag(v); n != "" {
					names = append(names, n)
					source = "label:" + key
					break
				}
			}
		}
	}

	if len(names) == 0 {
		logWarn("无法识别容器镜像名，该容器将被跳过（可通过 backup.provider 标签或 BACKUP_IMAGE_PATTERNS_FILE 显式指定）",
			"container", containerID)
	} else {
		logDebug("容器镜像识别完成", "container", containerID, "names", strings.Join(names, ","), "source", source)
	}

	dc.mu.Lock()
	dc.nameCache[containerID] = names
	dc.mu.Unlock()
	return names, nil
}

// containerBackupProviderLabel 读取容器上显式声明的 provider（如 backup.provider=postgres）。
func (dc *dockerClient) containerBackupProviderLabel(ctx context.Context, containerID string) string {
	inspect, err := dc.api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.Config == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(inspect.Container.Config.Labels[labelBackupProvider]))
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
	code, err := dc.execExitCode(ctx, execID)
	if err != nil {
		return false, err
	}

	dc.mu.Lock()
	if dc.binCache[containerID] == nil {
		dc.binCache[containerID] = map[string]bool{}
	}
	dc.binCache[containerID][binary] = code == 0
	dc.mu.Unlock()
	return code == 0, nil
}

// execExitCode 查询已执行进程的退出码（C2）。
func (dc *dockerClient) execExitCode(ctx context.Context, execID string) (int, error) {
	info, err := dc.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	return info.ExitCode, nil
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
	code, err := dc.execExitCode(ctx, execID)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("命令执行失败 (exit %d): %s", code, strings.TrimSpace(stderr.String()))
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
