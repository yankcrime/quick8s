package k3s

import (
	"fmt"

	"quick8s/internal/node"
)

// AgentOpts controls how the K3s agent install script is invoked on a
// worker node.
type AgentOpts struct {
	// Version is the K3s channel/version to install, e.g. "v1.30.4+k3s1".
	// Empty means "latest stable". Should match the control plane's version.
	Version string
	// ServerURL is the control plane's API address, e.g. "https://<host>:6443".
	ServerURL string
	// Token is the cluster join token, from NodeToken.
	Token string
	// NodeIP pins this worker's own advertised address - see
	// InstallOpts.NodeIP.
	NodeIP string
}

// JoinAgent installs K3s in agent mode on the target node, joining it to the
// cluster at ServerURL as a worker.
func JoinAgent(c *node.Client, opts AgentOpts) error {
	script := fmt.Sprintf("curl -sfL https://get.k3s.io | K3S_URL=%q K3S_TOKEN=%q", opts.ServerURL, opts.Token)
	if opts.Version != "" {
		script += fmt.Sprintf(" INSTALL_K3S_VERSION=%q", opts.Version)
	}
	script += " sh -s -"
	if opts.NodeIP != "" {
		script += fmt.Sprintf(" --node-ip %q", opts.NodeIP)
	}

	out, err := c.Run(script)
	if err != nil {
		return fmt.Errorf("joining worker: %w\n%s", err, out)
	}
	return nil
}
