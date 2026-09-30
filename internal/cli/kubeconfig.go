package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newKubeconfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubeconfig <target>",
		Short: "Retrieve the kubeconfig from an already-bootstrapped K3s node",
		Long:  "Fetch the kubeconfig from a K3s node, identified by hostname or IP, over SSH and print it to stdout.",
		Args:  cobra.ExactArgs(1),
	}

	ssh := registerSSHFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		target := ssh.target(args[0])

		client, err := node.Dial(target)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", target.Host, err)
		}
		defer client.Close()

		kubeconfig, err := k3s.Kubeconfig(client, target.Host)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), kubeconfig)
		return err
	}

	return cmd
}
