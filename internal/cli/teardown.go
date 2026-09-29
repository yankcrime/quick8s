package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newTeardownCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "teardown <target>",
		Short: "Uninstall K3s from a remote node",
		Long:  "Run K3s's own generated uninstall script on a remote node, identified by hostname or IP, over SSH. This removes K3s entirely and cannot be undone.",
		Args:  cobra.ExactArgs(1),
	}

	ssh := registerSSHFlags(cmd)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		target := ssh.target(args[0])

		if !yes {
			ok, err := confirm(cmd, fmt.Sprintf("This will remove K3s from %s. Continue?", target.Host))
			if err != nil {
				return err
			}
			if !ok {
				cmd.PrintErrln("Aborted.")
				return nil
			}
		}

		client, err := node.Dial(target)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", target.Host, err)
		}
		defer client.Close()

		cmd.PrintErrf("Connected to %s, uninstalling K3s...\n", target.Host)
		if err := k3s.Uninstall(client); err != nil {
			return err
		}

		cmd.PrintErrln("K3s uninstalled.")
		return nil
	}

	return cmd
}
