package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

type Manager struct {
	dataDir string
}

func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir}
}

type ContainerInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Image  string `json:"image"`
}

func (m *Manager) Create(ctx context.Context, cfg *types.AgentConfig) (*ContainerInfo, error) {
	args := []string{
		"create",
		"--name", "ha-" + string(cfg.ID),
		"--hostname", string(cfg.ID),
		"--restart", "no",
		"--stop-timeout", "10",
		"--memory", cfg.ResourceLimit.Memory,
		"--cpus", cfg.ResourceLimit.CPU,
		"--label", "homeagent.managed=true",
		"--label", "homeagent.agent-id=" + string(cfg.ID),
	}

	if !cfg.ResourceLimit.Network {
		args = append(args, "--network", "none")
	}

	args = append(args, cfg.Image)

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker create: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	id := strings.TrimSpace(string(out))
	return &ContainerInfo{ID: id, Name: "ha-" + string(cfg.ID), Status: "created", Image: cfg.Image}, nil
}

func (m *Manager) Start(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, "docker", "start", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker start: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, "docker", "stop", "--time", "5", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker stop: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) Remove(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) Inspect(ctx context.Context, containerID string) (*ContainerInfo, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", containerID)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}

	var containers []struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}

	if err := json.Unmarshal(out, &containers); err != nil {
		return nil, fmt.Errorf("parse inspect: %w", err)
	}

	if len(containers) == 0 {
		return nil, fmt.Errorf("container %s not found", containerID)
	}

	c := containers[0]
	return &ContainerInfo{
		ID:     c.ID,
		Name:   strings.TrimPrefix(c.Name, "/"),
		Status: c.State.Status,
		Image:  c.Config.Image,
	}, nil
}

func (m *Manager) WaitHealthy(ctx context.Context, containerID string) error {
	args := []string{
		"exec", containerID,
		"agentd", "--probe",
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.Run()
}

func (m *Manager) Exec(ctx context.Context, containerID string, cmdArgs []string) ([]byte, error) {
	args := append([]string{"exec", containerID}, cmdArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker exec: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}

func (m *Manager) Commit(ctx context.Context, containerID string, imageTag string) error {
	cmd := exec.CommandContext(ctx, "docker", "commit", containerID, imageTag)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker commit: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) SaveImage(ctx context.Context, imageTag string, outputPath string) error {
	cmd := exec.CommandContext(ctx, "docker", "save", "-o", outputPath, imageTag)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker save: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) LoadImage(ctx context.Context, inputPath string) error {
	cmd := exec.CommandContext(ctx, "docker", "load", "-i", inputPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker load: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (m *Manager) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=homeagent.managed=true",
		"--format", "{{.ID}}\t{{.Names}}\t{{.Status}}\t{{.Image}}",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		containers = append(containers, ContainerInfo{
			ID: parts[0], Name: parts[1], Status: parts[2], Image: parts[3],
		})
	}
	return containers, nil
}
