package k3s

import (
	"fmt"
	"strings"

	"quick8s/internal/shell"
)

const pendingUpdatePath = "/etc/rancher/k3s/.quick8s-update-pending"

// PendingUpdate records an update until its service passes readiness checks.
// An empty Version means a config-only restart; otherwise the installer must
// be re-run, even when it already replaced the binary on a previous attempt.
type PendingUpdate struct {
	Version string
}

// ReadPendingUpdate returns nil when the previous update completed.
func ReadPendingUpdate(c rootRunner) (*PendingUpdate, error) {
	out, err := c.RunAsRoot("if test -f " + pendingUpdatePath + "; then echo present; cat " + pendingUpdatePath + "; fi")
	if err != nil {
		return nil, fmt.Errorf("reading pending update: %w", err)
	}
	content, present := strings.CutPrefix(out, "present\n")
	if !present {
		return nil, nil
	}
	version := strings.TrimSpace(content)
	if version != "" {
		if _, err := ParseRelease(version); err != nil {
			return nil, fmt.Errorf("reading pending update: %w", err)
		}
	}
	return &PendingUpdate{Version: version}, nil
}

// BeginUpdate records intent before any config or binary changes. Rename
// makes the marker atomic if the SSH connection drops while writing it.
func BeginUpdate(c rootRunner, version string) error {
	_, err := c.RunAsRoot("mkdir -p /etc/rancher/k3s && printf '%s\\n' " + shell.Quote(version) +
		" > " + pendingUpdatePath + ".tmp && mv " + pendingUpdatePath + ".tmp " + pendingUpdatePath)
	if err != nil {
		return fmt.Errorf("recording pending update: %w", err)
	}
	return nil
}

// FinishUpdate clears the marker only after readiness has succeeded.
func FinishUpdate(c rootRunner) error {
	if _, err := c.RunAsRoot("rm -f " + pendingUpdatePath); err != nil {
		return fmt.Errorf("clearing pending update: %w", err)
	}
	return nil
}
