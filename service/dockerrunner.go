package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if bytes.Contains(ee.Stderr, []byte("No such object")) || bytes.Contains(ee.Stderr, []byte("No such container")) {
				return false, nil // container doesn't exist (ML offline package not installed/created): treat as not running, skip silently
			}
			return false, fmt.Errorf("docker inspect %s: %w: %s", name, err, ee.Stderr)
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func (dockerRunner) Restart(ctx context.Context, name string) error {
	// Allow 90s: 30s grace period to stop the container + cold-start bring-up
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "restart", "-t", "30", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker restart %s: %w: %s", name, err, out)
	}
	return nil
}
