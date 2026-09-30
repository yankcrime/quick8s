package cli

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newBootstrapCmd() *cobra.Command {
	var (
		k3sVersion    string
		configFile    string
		workers       []string
		controlPlanes []string
	)

	cmd := &cobra.Command{
		Use:   "bootstrap [target]",
		Short: "Bootstrap a K3s cluster",
		Long: "Bootstrap a K3s cluster over SSH.\n\n" +
			"For a single node, name it as the positional [target]. For anything with\n" +
			"more than one node - an HA control plane, or a control plane with workers -\n" +
			"every control plane node, including the first, is named with --control-plane;\n" +
			"the first --control-plane given is the one that initializes the cluster.",
		Args: cobra.MaximumNArgs(1),
	}

	ssh := registerSSHFlags(cmd)
	cmd.Flags().StringVar(&k3sVersion, "k3s-version", "", "K3s version to install, e.g. v1.30.4+k3s1 (defaults to latest stable)")
	cmd.Flags().StringVar(&configFile, "config-file", "", "path to a local K3s config.yaml to install on control plane nodes before starting K3s (see https://docs.k3s.io/installation/configuration#configuration-file)")
	cmd.Flags().StringSliceVar(&workers, "worker", nil, "hostname or IP of a worker node to join to the control plane (repeatable, or comma-separated)")
	cmd.Flags().StringSliceVar(&controlPlanes, "control-plane", nil, "hostname or IP of a control plane node; the first one given initializes the cluster, any more form an HA cluster with embedded etcd (repeatable, or comma-separated)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			if len(controlPlanes) > 0 || len(workers) > 0 {
				return fmt.Errorf("can't combine a positional target with --control-plane or --worker; use --control-plane %s instead", args[0])
			}
			controlPlanes = []string{args[0]}
		} else if len(controlPlanes) == 0 {
			return fmt.Errorf("specify a target host, or at least one --control-plane")
		}

		initiator, additionalControlPlanes := controlPlanes[0], controlPlanes[1:]
		target := ssh.target(initiator)

		client, err := node.Dial(target)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", target.Host, err)
		}
		defer client.Close()

		// Status goes to stderr so stdout stays clean for the
		// kubeconfig, e.g. `quick8s bootstrap host > kubeconfig.yaml`.
		cmd.PrintErrf("Connected to %s, running preflight checks...\n", target.Host)
		if err := k3s.Preflight(client); err != nil {
			return err
		}

		if configFile != "" {
			cmd.PrintErrf("Pushing config file %s to node...\n", configFile)
			if err := k3s.PushConfigFile(client, configFile); err != nil {
				return err
			}
		}

		cmd.PrintErrln("Installing K3s...")
		if err := k3s.Install(client, k3s.InstallOpts{
			Version:     k3sVersion,
			ClusterInit: len(additionalControlPlanes) > 0,
			TLSSAN:      target.Host,
			NodeIP:      nodeIPFor(target.Host),
		}); err != nil {
			return err
		}

		if len(additionalControlPlanes) > 0 || len(workers) > 0 {
			cmd.PrintErrln("Fetching node token for cluster join...")
			token, err := k3s.NodeToken(client)
			if err != nil {
				return err
			}

			serverURL := "https://" + net.JoinHostPort(target.Host, "6443")
			if err := joinControlPlanes(cmd, ssh, additionalControlPlanes, configFile, k3s.ControlPlaneOpts{
				Version:   k3sVersion,
				ServerURL: serverURL,
				Token:     token,
			}); err != nil {
				return err
			}

			if err := joinWorkers(cmd, ssh, workers, k3s.AgentOpts{
				Version:   k3sVersion,
				ServerURL: serverURL,
				Token:     token,
			}); err != nil {
				return err
			}
		}

		cmd.PrintErrln("Fetching kubeconfig...")
		kubeconfig, err := k3s.Kubeconfig(client, target.Host)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), kubeconfig)
		return err
	}

	return cmd
}
