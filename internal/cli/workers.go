package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// joinWorkers joins each host in workers to the cluster at opts.ServerURL as a
// K3s agent, using a node token already fetched from the control plane.
// Shared between `bootstrap --worker`, the standalone `join` command, and
// `up` (via addWorker, on connections it already holds).
func joinWorkers(cmd *cobra.Command, ssh *sshFlags, workers []string, config []byte, opts k3s.AgentOpts) error {
	for _, host := range workers {
		if err := joinWorker(cmd, ssh, host, config, opts); err != nil {
			return err
		}
	}

	return nil
}

func joinWorker(cmd *cobra.Command, ssh *sshFlags, host string, config []byte, opts k3s.AgentOpts) error {
	target := ssh.target(host)

	cmd.PrintErrf("Connecting to worker %s...\n", host)
	client, err := node.Dial(target)
	if err != nil {
		return fmt.Errorf("connecting to worker %s: %w", host, err)
	}
	defer client.Close()

	return addWorker(cmd, client, host, config, opts)
}

// addWorker joins an already-connected node as a worker: preflight,
// optional config, install.
func addWorker(cmd *cobra.Command, client *node.Client, host string, config []byte, opts k3s.AgentOpts) error {
	cmd.PrintErrf("Running preflight checks on worker %s...\n", host)
	if err := k3s.Preflight(client, indented(cmd)); err != nil {
		return fmt.Errorf("worker %s: %w", host, err)
	}

	if err := pushConfig(cmd, client, "worker", host, config); err != nil {
		return err
	}

	opts.NodeIP = nodeIPFor(host)
	if err := slowStep(cmd, fmt.Sprintf("Joining worker %s...", host), func(progress k3s.Progress) error {
		return k3s.JoinAgent(client, opts, progress)
	}); err != nil {
		return fmt.Errorf("worker %s: %w", host, err)
	}

	cmd.PrintErrf("Worker %s joined.\n", host)
	return nil
}
