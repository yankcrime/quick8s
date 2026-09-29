package k3s

import (
	"fmt"
	"os"

	"quick8s/internal/node"
)

// ConfigFilePath is where K3s itself looks for its configuration file
// (https://docs.k3s.io/installation/configuration#configuration-file). It's
// read directly by the k3s service, not by the install script, so it must
// be in place before K3s first starts.
const ConfigFilePath = "/etc/rancher/k3s/config.yaml"

// PushConfigFile uploads a local K3s config file to the node at
// ConfigFilePath, ahead of Install, so the k3s service picks it up on its
// first start.
func PushConfigFile(c *node.Client, localPath string) error {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("reading config file %s: %w", localPath, err)
	}

	if err := c.WriteFile(ConfigFilePath, content); err != nil {
		return fmt.Errorf("pushing config file to node: %w", err)
	}
	return nil
}
