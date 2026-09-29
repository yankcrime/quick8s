package k3s

import (
	"fmt"
	"strings"

	"quick8s/internal/node"
)

const nodeTokenPath = "/var/lib/rancher/k3s/server/node-token"

// NodeToken fetches the cluster's join token from a control plane node.
// Any node holding this token can join the cluster, so it's only ever read
// over the SSH connection quick8s itself established.
func NodeToken(c *node.Client) (string, error) {
	// Root-only, same fallback as Kubeconfig: works whether the SSH user is
	// root already or has passwordless sudo.
	cmd := fmt.Sprintf("sudo -n cat %s 2>/dev/null || cat %s", nodeTokenPath, nodeTokenPath)
	out, err := c.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("fetching node token: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}
