package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// joinControlPlanes joins each host in controlPlanes to the cluster at
// opts.ServerURL as an additional K3s server node, forming an HA embedded-etcd
// cluster. The initiating server must already have been
// installed with ClusterInit for this to work.
func joinControlPlanes(cmd *cobra.Command, ssh *sshFlags, controlPlanes []string, configFile string, opts k3s.ControlPlaneOpts) error {
	for _, host := range controlPlanes {
		if err := joinControlPlane(cmd, ssh, host, configFile, opts); err != nil {
			return err
		}
	}

	return nil
}

func joinControlPlane(cmd *cobra.Command, ssh *sshFlags, host, configFile string, opts k3s.ControlPlaneOpts) error {
	target := ssh.target(host)

	cmd.PrintErrf("Connecting to control plane %s...\n", host)
	client, err := node.Dial(target)
	if err != nil {
		return fmt.Errorf("connecting to control plane %s: %w", host, err)
	}
	defer client.Close()

	cmd.PrintErrf("Running preflight checks on control plane %s...\n", host)
	if err := k3s.Preflight(client, indented(cmd)); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	if configFile != "" {
		cmd.PrintErrf("Pushing config file to control plane %s...\n", host)
		if err := k3s.PushConfigFile(client, configFile); err != nil {
			return fmt.Errorf("control plane %s: %w", host, err)
		}
	}

	opts.TLSSAN = host
	opts.NodeIP = nodeIPFor(host)
	if err := slowStep(cmd, fmt.Sprintf("Joining control plane %s...", host), func(progress k3s.Progress) error {
		return k3s.JoinControlPlane(client, opts, progress)
	}); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	cmd.PrintErrf("Control plane %s joined.\n", host)
	return nil
}
