package k3s

import (
	"fmt"
	"strings"
)

// configFilePath is where K3s itself looks for its configuration file
// (https://docs.k3s.io/installation/configuration#configuration-file). It's
// read directly by the k3s service, not by the install script, so it must
// be in place before K3s first starts.
const configFilePath = "/etc/rancher/k3s/config.yaml"

// PushConfig uploads K3s config file content to the node at configFilePath,
// ahead of installing, so the k3s service picks it up on its first start.
// K3s only reads it on start, so a node already running needs a Restart.
func PushConfig(c rootFileWriter, content []byte) error {
	if err := c.WriteFileAsRoot(configFilePath, content); err != nil {
		return fmt.Errorf("pushing config file to node: %w", err)
	}
	return nil
}

// ReadConfig returns the node's K3s config file, or nil if it has none.
func ReadConfig(c rootRunner) ([]byte, error) {
	// The marker line tells a missing file apart from an empty one.
	out, err := c.RunAsRoot(fmt.Sprintf("if test -f %[1]s; then echo present; cat %[1]s; fi", configFilePath))
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	content, ok := strings.CutPrefix(out, "present\n")
	if !ok {
		return nil, nil
	}
	return []byte(content), nil
}

// RemoveConfig deletes the node's K3s config file, if any.
func RemoveConfig(c rootRunner) error {
	if out, err := c.RunAsRoot("rm -f " + configFilePath); err != nil {
		return fmt.Errorf("removing config file: %w\n%s", err, out)
	}
	return nil
}
