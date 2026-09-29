package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newJoinCmd() *cobra.Command {
	var (
		k3sVersion string
		workers    []string
	)

	cmd := &cobra.Command{
		Use:   "join <control-plane>",
		Short: "Join worker nodes to an existing K3s control plane",
		Long:  "Join one or more worker nodes, identified by hostname or IP, to an already-bootstrapped K3s control plane over SSH.",
		Args:  cobra.ExactArgs(1),
	}

	ssh := registerSSHFlags(cmd)
	cmd.Flags().StringVar(&k3sVersion, "k3s-version", "", "K3s version to install on workers, e.g. v1.30.4+k3s1 (defaults to latest stable; should match the control plane's version)")
	cmd.Flags().StringSliceVar(&workers, "worker", nil, "hostname or IP of a worker node to join to the control plane (repeatable, or comma-separated, required)")
	_ = cmd.MarkFlagRequired("worker")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		serverHost := args[0]
		target := ssh.target(serverHost)

		cmd.PrintErrf("Connecting to control plane %s...\n", serverHost)
		client, err := node.Dial(target)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", serverHost, err)
		}
		defer client.Close()

		cmd.PrintErrln("Fetching node token...")
		token, err := k3s.NodeToken(client)
		if err != nil {
			return err
		}

		return joinWorkers(cmd, ssh, serverHost, token, k3sVersion, workers)
	}

	return cmd
}
