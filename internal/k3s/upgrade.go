package k3s

import "fmt"

// Upgrade re-runs the K3s installer on a node that already runs K3s, which
// is K3s's own manual upgrade path: the installer replaces the binary with
// opts.Version and restarts the service. The installer also rewrites the
// service from its environment and arguments, so opts must repeat how the
// node was installed - its role, server URL, token, and addresses - or the
// node comes back without them.
func Upgrade(c rootStreamer, opts Options, progress Progress) error {
	inv := opts.invocation()
	// A previous attempt may have installed the binary and service files
	// without successfully starting K3s. The installer otherwise skips an
	// unchanged installation, leaving that failed update unrecoverable.
	inv.env = append(inv.env, "INSTALL_K3S_FORCE_RESTART=true")
	if err := runInstall(c, inv, progress); err != nil {
		return fmt.Errorf("upgrading k3s: %w", err)
	}
	return nil
}

// Restart restarts the K3s service on a node, e.g. to pick up a changed
// config file. Call WaitReady afterwards before changing another node;
// OpenRC returning successfully does not mean K3s is ready.
func Restart(c rootRunner, installation Installation) error {
	service := "k3s"
	if installation == InstalledAgent {
		service = "k3s-agent"
	}
	// The installer sets K3s up under systemd, or OpenRC where there's no
	// systemd.
	out, err := c.RunAsRoot(fmt.Sprintf("if command -v systemctl >/dev/null 2>&1; then systemctl restart %[1]s; else rc-service %[1]s restart; fi", service))
	if err != nil {
		return fmt.Errorf("restarting %s: %w\n%s", service, err, out)
	}
	return nil
}
