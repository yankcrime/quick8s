package k3s

import (
	"fmt"
	"strings"

	"quick8s/internal/node"
)

const uninstallScriptPath = "/usr/local/bin/k3s-uninstall.sh"

// Uninstall runs K3s's own generated uninstall script on the target node,
// removing the service, binaries, and data.
func Uninstall(c *node.Client) error {
	if out, err := c.Run(fmt.Sprintf("test -x %s", uninstallScriptPath)); err != nil {
		return fmt.Errorf("k3s does not appear to be installed on this node (no %s): %s", uninstallScriptPath, strings.TrimSpace(out))
	}

	// Mirror the sudo fallback used elsewhere: works whether the SSH user is
	// root already or has passwordless sudo.
	cmd := fmt.Sprintf("sudo -n %s 2>&1 || %s", uninstallScriptPath, uninstallScriptPath)
	out, err := c.Run(cmd)
	if err != nil {
		return fmt.Errorf("uninstalling k3s: %w\n%s", err, out)
	}
	return nil
}
