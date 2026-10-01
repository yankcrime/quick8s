package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// initControlPlane installs the K3s server that initializes a new cluster
// on an already-connected node: preflight, optional config, install.
func initControlPlane(cmd *cobra.Command, client *node.Client, host string, config []byte, opts k3s.InstallOpts) error {
	cmd.PrintErrf("Running preflight checks on %s...\n", host)
	if err := k3s.Preflight(client, indented(cmd)); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	if err := pushConfig(cmd, client, "control plane", host, config); err != nil {
		return err
	}

	opts.TLSSAN = host
	opts.NodeIP = nodeIPFor(host)
	if err := slowStep(cmd, fmt.Sprintf("Installing K3s on %s...", host), func(progress k3s.Progress) error {
		return k3s.Install(client, opts, progress)
	}); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}
	return nil
}

// joinControlPlanes joins each host in controlPlanes to the cluster at
// opts.ServerURL as an additional K3s server node, forming an HA embedded-etcd
// cluster. The initiating server must already have been
// installed with ClusterInit for this to work.
func joinControlPlanes(cmd *cobra.Command, ssh *sshFlags, controlPlanes []string, config []byte, opts k3s.ControlPlaneOpts) error {
	for _, host := range controlPlanes {
		if err := joinControlPlane(cmd, ssh, host, config, opts); err != nil {
			return err
		}
	}

	return nil
}

func joinControlPlane(cmd *cobra.Command, ssh *sshFlags, host string, config []byte, opts k3s.ControlPlaneOpts) error {
	target := ssh.target(host)

	cmd.PrintErrf("Connecting to control plane %s...\n", host)
	client, err := node.Dial(target)
	if err != nil {
		return fmt.Errorf("connecting to control plane %s: %w", host, err)
	}
	defer client.Close()

	return addControlPlane(cmd, client, host, config, opts)
}

// addControlPlane joins an already-connected node as an additional control
// plane: preflight, optional config, install.
func addControlPlane(cmd *cobra.Command, client *node.Client, host string, config []byte, opts k3s.ControlPlaneOpts) error {
	cmd.PrintErrf("Running preflight checks on control plane %s...\n", host)
	if err := k3s.Preflight(client, indented(cmd)); err != nil {
		return fmt.Errorf("control plane %s: %w", host, err)
	}

	if err := pushConfig(cmd, client, "control plane", host, config); err != nil {
		return err
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

// pushConfig uploads K3s config.yaml content ahead of install, if any.
func pushConfig(cmd *cobra.Command, client *node.Client, role, host string, config []byte) error {
	if config == nil {
		return nil
	}
	cmd.PrintErrf("Pushing K3s config to %s %s...\n", role, host)
	if err := k3s.PushConfig(client, config); err != nil {
		return fmt.Errorf("%s %s: %w", role, host, err)
	}
	return nil
}
