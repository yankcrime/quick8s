package k3s

import (
	"fmt"
	"net"
	"strings"
)

const kubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// Kubeconfig fetches the K3s-generated kubeconfig from the remote node and
// rewrites its server address from 127.0.0.1 to host, so the file is usable
// from outside the node.
func Kubeconfig(c rootRunner, host string) (string, error) {
	out, err := c.RunAsRoot("cat " + kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("fetching kubeconfig: %w\n%s", err, out)
	}

	rewritten := strings.Replace(out, "https://127.0.0.1:6443", "https://"+net.JoinHostPort(host, "6443"), 1)
	return rewritten, nil
}
