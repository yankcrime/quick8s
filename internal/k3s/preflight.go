package k3s

import (
	"fmt"
	"strings"
)

// Preflight runs basic checks against the target before attempting install:
// that we have root (or passwordless sudo), and that K3s isn't already
// installed. Each check is reported to progress as it passes.
func Preflight(c preflightRunner, progress Progress) error {
	if _, err := c.RunAsRoot("true"); err != nil {
		return fmt.Errorf("checking root access (root or passwordless sudo required): %w", err)
	}
	uid, err := c.Run("id -u")
	if err != nil {
		return fmt.Errorf("checking remote user: %w", err)
	}
	if strings.TrimSpace(uid) == "0" {
		progress.printf("root access: ok (connected as root)")
	} else {
		progress.printf("root access: ok (passwordless sudo)")
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
	progress.printf("existing K3s installation: none")

	return nil
}
