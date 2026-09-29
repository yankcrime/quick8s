package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// joinWorkers joins each host in workers to the cluster at serverHost as a
// K3s agent, using a node token already fetched from the control plane.
// Shared between `bootstrap --worker` and the standalone `join` command.
func joinWorkers(cmd *cobra.Command, ssh *sshFlags, serverHost, token, k3sVersion string, workers []string) error {
	serverURL := fmt.Sprintf("https://%s:6443", serverHost)

	for _, host := range workers {
		if err := joinWorker(cmd, ssh, host, serverURL, token, k3sVersion); err != nil {
			return err
		}
	}

	return nil
}

func joinWorker(cmd *cobra.Command, ssh *sshFlags, host, serverURL, token, k3sVersion string) error {
	target := ssh.target(host)

	cmd.PrintErrf("Connecting to worker %s...\n", host)
	client, err := node.Dial(target)
	if err != nil {
		return fmt.Errorf("connecting to worker %s: %w", host, err)
	}
	defer client.Close()

	if err := k3s.Preflight(client); err != nil {
		return fmt.Errorf("worker %s: %w", host, err)
	}

	cmd.PrintErrf("Joining worker %s...\n", host)
	if err := k3s.JoinAgent(client, k3s.AgentOpts{Version: k3sVersion, ServerURL: serverURL, Token: token, NodeIP: nodeIPFor(host)}); err != nil {
		return fmt.Errorf("worker %s: %w", host, err)
	}

	cmd.PrintErrf("Worker %s joined.\n", host)
	return nil
}
