package cli

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or grow the cluster defined in quick8s.yaml",
		Long: "Read the cluster definition (quick8s.yaml in the current directory, or --file)\n" +
			"and install K3s on every node that doesn't have it yet: the first control\n" +
			"plane initializes the cluster, and the rest of the nodes join it. Nodes\n" +
			"already running K3s in their listed role are left untouched, so running up\n" +
			"again after adding nodes to the file joins just the new ones.\n\n" +
			"Every node is checked over SSH before anything is installed. The kubeconfig\n" +
			"is printed to stdout, e.g. `quick8s up > kubeconfig.yaml`.",
		Args: cobra.NoArgs,
	}

	file := registerFileFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now; failures from here on aren't usage errors.
		cmd.SilenceUsage = true
		spec, err := cluster.Load(*file)
		if err != nil {
			return err
		}
		ssh := specSSH(spec.SSH)

		// Connect to and inspect every node before changing any of them, so
		// an unreachable node or a role mismatch fails without side effects.
		// Connections stay open for the install steps below.
		cmd.PrintErrf("Checking nodes in %s...\n", *file)
		clients := map[string]*node.Client{}
		defer func() {
			for _, c := range clients {
				c.Close()
			}
		}()
		installed := map[string]k3s.Installation{}
		for _, n := range spec.Nodes() {
			client, err := node.Dial(ssh.target(n.Host))
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", n.Host, err)
			}
			clients[n.Host] = client
			if installed[n.Host], err = k3s.DetectInstallation(client); err != nil {
				return fmt.Errorf("%s: %w", n.Host, err)
			}
			cmd.PrintErrf("  %s: %s\n", n.Host, installed[n.Host])
		}

		plan, err := spec.Plan(installed)
		if err != nil {
			return err
		}
		server := clients[plan.Server]

		if plan.InitServer {
			if err := initControlPlane(cmd, server, plan.Server, spec.ServerConfig, k3s.InstallOpts{
				Version:     spec.KubernetesVersion,
				ClusterInit: len(spec.ControlPlanes) > 1,
			}); err != nil {
				return err
			}
		} else if len(plan.JoinControlPlanes) > 0 {
			etcd, err := k3s.EmbeddedEtcd(server)
			if err != nil {
				return fmt.Errorf("control plane %s: %w", plan.Server, err)
			}
			if !etcd {
				return fmt.Errorf("can't add control planes: %s runs K3s's single-server sqlite datastore, not embedded etcd; "+
					"clusters that may grow beyond one control plane need `cluster-init: true` under k3s.server when first created", plan.Server)
			}
		}

		// Joining nodes install the version the cluster actually runs, not
		// kubernetesVersion: up never upgrades existing nodes, and a node
		// newer than its control plane is outside Kubernetes' skew policy.
		running, err := k3s.Version(server)
		if err != nil {
			return fmt.Errorf("control plane %s: %w", plan.Server, err)
		}
		if !plan.InitServer && spec.KubernetesVersion != "" && spec.KubernetesVersion != running {
			cmd.PrintErrf("Warning: the cluster runs K3s %s, not cluster.kubernetesVersion %s; up doesn't upgrade existing nodes, so new nodes will install %s to match.\n",
				running, spec.KubernetesVersion, running)
		}

		if len(plan.JoinControlPlanes) > 0 || len(plan.JoinWorkers) > 0 {
			cmd.PrintErrf("Fetching node token from %s...\n", plan.Server)
			token, err := k3s.NodeToken(server)
			if err != nil {
				return err
			}
			serverURL := "https://" + net.JoinHostPort(plan.Server, "6443")

			for _, host := range plan.JoinControlPlanes {
				if err := addControlPlane(cmd, clients[host], host, spec.ServerConfig, k3s.ControlPlaneOpts{
					Version:   running,
					ServerURL: serverURL,
					Token:     token,
				}); err != nil {
					return err
				}
			}
			for _, host := range plan.JoinWorkers {
				if err := addWorker(cmd, clients[host], host, spec.AgentConfig, k3s.AgentOpts{
					Version:   running,
					ServerURL: serverURL,
					Token:     token,
				}); err != nil {
					return err
				}
			}
		}

		if plan.Empty() {
			cmd.PrintErrln("All nodes already match the cluster definition; nothing to do.")
		} else {
			cmd.PrintErrf("Cluster is up, running K3s %s.\n", running)
		}

		kubeconfig, err := k3s.Kubeconfig(server, plan.Server)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), kubeconfig)
		return err
	}

	return cmd
}

// registerFileFlag adds the --file flag shared by commands that read a
// cluster definition.
func registerFileFlag(cmd *cobra.Command) *string {
	return cmd.Flags().StringP("file", "f", cluster.DefaultFile, "path to the cluster definition")
}
