package k3s

import (
	"fmt"
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
func JoinAgent(c rootStreamer, opts AgentOpts, progress Progress) error {
	env := []string{"K3S_URL=" + opts.ServerURL, "K3S_TOKEN=" + opts.Token}
	args := []string{"agent"}
	if opts.NodeIP != "" {
		args = append(args, "--node-ip", opts.NodeIP)
	}

	if err := runInstall(c, opts.Version, env, args, progress); err != nil {
		return fmt.Errorf("joining worker: %w", err)
	}
	return nil
}
