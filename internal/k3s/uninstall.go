package k3s

import (
	"fmt"
	"strings"
)

const uninstallScriptPath = "/usr/local/bin/k3s-uninstall.sh"

// Uninstall runs K3s's own generated uninstall script on the target node,
// removing the service, binaries, and data.
func Uninstall(c rootRunner) error {
	out, err := c.RunAsRoot(fmt.Sprintf("if test -x %s; then printf installed; fi", uninstallScriptPath))
	if err != nil {
		return fmt.Errorf("checking for k3s uninstall script: %w", err)
	}
	if strings.TrimSpace(out) != "installed" {
		return fmt.Errorf("k3s does not appear to be installed on this node (no %s)", uninstallScriptPath)
	}

	out, err = c.RunAsRoot(uninstallScriptPath)
	if err != nil {
		return fmt.Errorf("uninstalling k3s: %w\n%s", err, out)
	}
	return nil
}
