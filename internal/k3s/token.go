package k3s

import (
	"fmt"
	"strings"
)

const nodeTokenPath = "/var/lib/rancher/k3s/server/node-token"

// NodeToken fetches the cluster's join token from a control plane node.
// Any node holding this token can join the cluster, so it's only ever read
// over the SSH connection quick8s itself established.
func NodeToken(c rootRunner) (string, error) {
	out, err := c.RunAsRoot("cat " + nodeTokenPath)
	if err != nil {
		return "", fmt.Errorf("fetching node token: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}
