package k3s

import (
	"fmt"
	"strings"

	"quick8s/internal/node"
)

// Preflight runs basic checks against the target before attempting install:
// that we have root (or passwordless sudo), and that K3s isn't already
// installed.
func Preflight(c *node.Client) error {
	out, err := c.Run("id -u")
	if err != nil {
		return fmt.Errorf("checking remote user: %w", err)
	}
	if strings.TrimSpace(out) != "0" {
		if _, err := c.Run("sudo -n true"); err != nil {
			return fmt.Errorf("remote user is not root and passwordless sudo is unavailable")
		}
	}

	if out, err := c.Run("command -v k3s"); err == nil && strings.TrimSpace(out) != "" {
		return fmt.Errorf("k3s already appears to be installed at %s; tear it down first if you want to reinstall", strings.TrimSpace(out))
	}

	return nil
}
