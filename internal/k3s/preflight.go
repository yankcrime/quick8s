package k3s

import (
	"fmt"
	"strings"
)

// Preflight runs basic checks against the target before attempting install:
// that we have root (or passwordless sudo), and that K3s isn't already
// installed.
func Preflight(c preflightRunner) error {
	if _, err := c.RunAsRoot("true"); err != nil {
		return fmt.Errorf("checking root access (root or passwordless sudo required): %w", err)
	}

	// An absent binary is an expected result, not a command failure. Keep
	// transport/session failures distinct instead of treating them as absence.
	out, err := c.Run("if command -v k3s >/dev/null 2>&1; then command -v k3s; fi")
	if err != nil {
		return fmt.Errorf("checking for existing k3s installation: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("k3s already appears to be installed at %s; tear it down first if you want to reinstall", strings.TrimSpace(out))
	}

	return nil
}
