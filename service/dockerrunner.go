package service

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// dockerRunner drives the docker CLI. The photos service runs as root under
// systemd, so it has docker access without extra group setup.
type dockerRunner struct{}

func (dockerRunner) IsRunning(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func (dockerRunner) Restart(ctx context.Context, name string) error {
	// 给足 90s:停容器 30s 宽限 + 冷启动拉起
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "restart", "-t", "30", name).Run()
}
