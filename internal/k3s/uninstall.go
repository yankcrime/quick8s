package k3s

import (
	"errors"
	"fmt"
)

// ErrNotInstalled is returned by Uninstall when the node has no K3s
// installation to remove.
var ErrNotInstalled = errors.New("k3s does not appear to be installed on this node")

// Uninstall runs K3s's own generated uninstall script on the target node,
// removing the service, binaries, and data. Servers and agents each get
// their own script.
func Uninstall(c preflightRunner) error {
	installation, err := DetectInstallation(c)
	if err != nil {
		return err
	}

	var script string
	switch installation {
	case InstalledServer:
		script = uninstallScriptPath
	case InstalledAgent:
		script = agentUninstallScriptPath
	default:
		return fmt.Errorf("%w (no %s or %s)", ErrNotInstalled, uninstallScriptPath, agentUninstallScriptPath)
	}

	out, err := c.RunAsRoot(script)
	if err != nil {
		return fmt.Errorf("uninstalling k3s: %w\n%s", err, out)
	}
	return nil
}
