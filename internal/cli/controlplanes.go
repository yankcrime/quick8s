package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// joinControlPlanes joins each host in controlPlanes to the cluster at
// serverHost as an additional K3s server node, forming an HA embedded-etcd
// cluster together with serverHost. serverHost must already have been
// installed with ClusterInit for this to work.
func joinControlPlanes(cmd *cobra.Command, ssh *sshFlags, serverHost, token, k3sVersion, configFile string, controlPlanes []string) error {
	serverURL := fmt.Sprintf("https://%s:6443", serverHost)

	for _, host := range controlPlanes {
		if err := joinControlPlane(cmd, ssh, host, serverURL, token, k3sVersion, configFile); err != nil {
			return err
		}
	}

	return nil
}

func joinControlPlane(cmd *cobra.Command, ssh *sshFlags, host, serverURL, token, k3sVersion, configFile string) error {
	target := ssh.target(host)

	cmd.PrintErrf("Connecting to control plane %s...\n", host)
	client, err := node.Dial(target)
	if err != nil {
		return fmt.Errorf("connecting to control plane %s: %w", host, err)
	}
	defer client.Close()

	if err := k3s.Preflight(client); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	if configFile != "" {
		cmd.PrintErrf("Pushing config file to control plane %s...\n", host)
		if err := k3s.PushConfigFile(client, configFile); err != nil {
			return fmt.Errorf("control plane %s: %w", host, err)
		}
	}

	cmd.PrintErrf("Joining control plane %s...\n", host)
	if err := k3s.JoinControlPlane(client, k3s.ControlPlaneOpts{Version: k3sVersion, ServerURL: serverURL, Token: token, TLSSan: host, NodeIP: nodeIPFor(host)}); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	cmd.PrintErrf("Control plane %s joined.\n", host)
	return nil
}
