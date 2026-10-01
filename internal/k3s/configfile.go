package k3s

import (
	"fmt"
)

// configFilePath is where K3s itself looks for its configuration file
// (https://docs.k3s.io/installation/configuration#configuration-file). It's
// read directly by the k3s service, not by the install script, so it must
// be in place before K3s first starts.
const configFilePath = "/etc/rancher/k3s/config.yaml"

// PushConfig uploads K3s config file content to the node at configFilePath,
// ahead of installing, so the k3s service picks it up on its first start.
func PushConfig(c rootFileWriter, content []byte) error {
	if err := c.WriteFileAsRoot(configFilePath, content); err != nil {
		return fmt.Errorf("pushing config file to node: %w", err)
	}
	return nil
}
