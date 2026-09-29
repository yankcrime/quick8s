package k3s

import (
	"fmt"
	"strings"

	"quick8s/internal/node"
)

const kubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// Kubeconfig fetches the K3s-generated kubeconfig from the remote node and
// rewrites its server address from 127.0.0.1 to host, so the file is usable
// from outside the node.
func Kubeconfig(c *node.Client, host string) (string, error) {
	// k3s.yaml is root-only; fall back to a plain read in case we're
	// already root and sudo isn't installed on a minimal image.
	cmd := fmt.Sprintf("sudo -n cat %s 2>/dev/null || cat %s", kubeconfigPath, kubeconfigPath)
	out, err := c.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("fetching kubeconfig: %w\n%s", err, out)
	}

	rewritten := strings.Replace(out, "https://127.0.0.1:6443", fmt.Sprintf("https://%s:6443", host), 1)
	return rewritten, nil
}
