package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newDownCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Uninstall K3s from every node defined in quick8s.yaml",
		Long: "Read the cluster definition (quick8s.yaml in the current directory, or --file)\n" +
			"and run K3s's own uninstall script on every node in it, workers first. This\n" +
			"destroys the cluster and its data and cannot be undone. Nodes without K3s\n" +
			"are skipped, and a node that fails doesn't stop the others from being removed.",
		Args: cobra.NoArgs,
	}

	file := registerFileFlag(cmd)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now; failures from here on aren't usage errors.
		cmd.SilenceUsage = true
		spec, err := cluster.Load(*file)
		if err != nil {
			return err
		}
		ssh := specSSH(spec.SSH)

		// Workers first, then control planes in reverse, so the node that
		// initialized the cluster goes last.
		controlPlanes := slices.Clone(spec.ControlPlanes)
		slices.Reverse(controlPlanes)
		nodes := append(slices.Clone(spec.Workers), controlPlanes...)

		if !yes {
			hosts := make([]string, len(nodes))
			for i, n := range nodes {
				hosts[i] = n.Host
			}
			ok, err := confirm(cmd, fmt.Sprintf("This will remove K3s and all cluster data from %d node(s): %s. Continue?",
				len(nodes), strings.Join(hosts, ", ")))
			if err != nil {
				return err
			}
			if !ok {
				cmd.PrintErrln("Aborted.")
				return nil
			}
		}

		var errs []error
		for _, n := range nodes {
			if err := removeNode(cmd, ssh, n.Host); err != nil {
				cmd.PrintErrf("  failed: %s\n", n.Host)
				errs = append(errs, fmt.Errorf("%s: %w", n.Host, err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("K3s wasn't removed from %d of %d node(s):\n%w", len(errs), len(nodes), errors.Join(errs...))
		}

		cmd.PrintErrln("Cluster removed.")
		return nil
	}

	return cmd
}

// removeNode uninstalls K3s from host, treating a node without K3s as
// already removed so an interrupted down can simply be re-run.
func removeNode(cmd *cobra.Command, ssh *sshFlags, host string) error {
	cmd.PrintErrf("Uninstalling K3s from %s...\n", host)
	client, err := node.Dial(ssh.target(host))
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer client.Close()

	err = k3s.Uninstall(client)
	if errors.Is(err, k3s.ErrNotInstalled) {
		cmd.PrintErrln("  not installed, skipping")
		return nil
	}
	if err != nil {
		return err
	}
	cmd.PrintErrln("  done")
	return nil
}
