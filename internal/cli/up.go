package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newUpCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create the cluster defined in quick8s.yaml, or bring it in line with it",
		Long: "Read the cluster definition (quick8s.yaml in the current directory, or --file)\n" +
			"and make the nodes match it. Nodes without K3s get it installed: the first\n" +
			"control plane initializes the cluster, and the rest of the nodes join it.\n" +
			"Nodes already running K3s are upgraded to cluster.kubernetesVersion, and get\n" +
			"their K3s config replaced when the k3s section for their role has changed,\n" +
			"one node at a time, control planes first. Nodes that already match are left\n" +
			"untouched, so up is safe to re-run.\n\n" +
			"Every node is checked over SSH, and every upgrade validated, before anything\n" +
			"changes. Changes to running nodes restart K3s on them, so up asks first\n" +
			"(skip with --yes). The kubeconfig is printed to stdout, e.g.\n" +
			"`quick8s up > kubeconfig.yaml`.",
		Args: cobra.NoArgs,
	}

	file := registerFileFlag(cmd)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirming changes to nodes already running K3s")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now; failures from here on aren't usage errors.
		cmd.SilenceUsage = true
		spec, err := cluster.Load(*file)
		if err != nil {
			return err
		}
		ssh := specSSH(spec.SSH)

		// Connect to and inspect every node before changing any of them, so
		// an unreachable node, a role mismatch, or an unsupported upgrade
		// fails without side effects. Connections stay open for the
		// changes below.
		cmd.PrintErrf("Checking nodes in %s...\n", *file)
		clients := map[string]*node.Client{}
		defer func() {
			for _, c := range clients {
				c.Close()
			}
		}()
		states := map[string]cluster.NodeState{}
		for _, n := range spec.Nodes() {
			client, err := node.Dial(ssh.target(n.Host))
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", n.Host, err)
			}
			clients[n.Host] = client
			state, err := probe(client)
			if err != nil {
				return fmt.Errorf("%s: %w", n.Host, err)
			}
			states[n.Host] = state
			if state.Version != "" {
				cmd.PrintErrf("  %s: %s (%s)\n", n.Host, state.Installation, state.Version)
			} else {
				cmd.PrintErrf("  %s: %s\n", n.Host, state.Installation)
			}
		}

		plan, err := spec.Plan(states)
		if err != nil {
			return err
		}
		if len(plan.Update) > 0 {
			cmd.PrintErrln("Nodes to change in place, one at a time:")
			for _, u := range plan.Update {
				cmd.PrintErrf("  %s: %s\n", u.Host, describeUpdate(spec, u))
			}
			if !yes {
				ok, err := confirm(cmd, "Each change restarts K3s on its node. Continue?")
				if err != nil {
					return err
				}
				if !ok {
					cmd.PrintErrln("Aborted.")
					return nil
				}
			}
		}
		server := clients[plan.Server]

		if plan.InitServer {
			if err := initControlPlane(cmd, server, plan.Server, spec.ServerConfig, k3s.InstallOpts{
				Version:     spec.KubernetesVersion,
				ClusterInit: len(spec.ControlPlanes) > 1,
			}); err != nil {
				return err
			}
		}

		var r reinstall
		if len(plan.Update) > 0 || len(plan.JoinControlPlanes) > 0 || len(plan.JoinWorkers) > 0 {
			needToken := len(plan.JoinControlPlanes) > 0 || len(plan.JoinWorkers) > 0
			for _, u := range plan.Update {
				needToken = needToken || u.Upgrade() && u.Host != plan.Server
			}
			if r, err = newReinstall(cmd, server, plan.Server, needToken); err != nil {
				return err
			}
		}
		if len(plan.JoinControlPlanes) > 0 && !plan.InitServer && !r.clusterInit {
			return fmt.Errorf("can't add control planes: %s runs K3s's single-server sqlite datastore, not embedded etcd; "+
				"clusters that may grow beyond one control plane need `cluster-init: true` under k3s.server when first created", plan.Server)
		}

		for _, u := range plan.Update {
			if err := applyUpdate(cmd, clients[u.Host], u, spec.Config(u.ControlPlane), r.options(u.Host, u.ControlPlane, u.To)); err != nil {
				return err
			}
		}

		// Joining nodes install the version the cluster actually runs:
		// that's kubernetesVersion once the upgrades above are done, and
		// otherwise whatever the cluster was created with, since a node
		// newer than its control plane is outside Kubernetes' skew policy.
		running, err := k3s.Version(server)
		if err != nil {
			return fmt.Errorf("control plane %s: %w", plan.Server, err)
		}
		for _, host := range plan.JoinControlPlanes {
			if err := addControlPlane(cmd, clients[host], host, spec.ServerConfig, k3s.ControlPlaneOpts{
				Version:   running,
				ServerURL: r.serverURL(),
				Token:     r.token,
			}); err != nil {
				return err
			}
		}
		for _, host := range plan.JoinWorkers {
			if err := addWorker(cmd, clients[host], host, spec.AgentConfig, k3s.AgentOpts{
				Version:   running,
				ServerURL: r.serverURL(),
				Token:     r.token,
			}); err != nil {
				return err
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

// probe finds out what K3s, if any, an already-connected node runs.
func probe(client *node.Client) (cluster.NodeState, error) {
	var s cluster.NodeState
	var err error
	if s.Installation, err = k3s.DetectInstallation(client); err != nil || s.Installation == k3s.NotInstalled {
		return s, err
	}
	if s.Version, err = k3s.Version(client); err != nil {
		return s, err
	}
	if s.Config, err = k3s.ReadConfig(client); err != nil {
		return s, err
	}
	s.Pending, err = k3s.ReadPendingUpdate(client)
	return s, err
}

// describeUpdate summarizes an in-place change for confirmation.
func describeUpdate(spec *cluster.Spec, u cluster.Update) string {
	var changes []string
	if u.Upgrade() {
		changes = append(changes, fmt.Sprintf("upgrade K3s %s -> %s", u.From, u.To))
	}
	switch {
	case u.Config && spec.Config(u.ControlPlane) == nil:
		changes = append(changes, "remove K3s config")
	case u.Config:
		changes = append(changes, "update K3s config")
	}
	if u.Restart && len(changes) == 0 {
		changes = append(changes, "retry pending K3s restart")
	}
	return strings.Join(changes, ", ")
}

// registerFileFlag adds the --file flag shared by commands that read a
// cluster definition.
func registerFileFlag(cmd *cobra.Command) *string {
	return cmd.Flags().StringP("file", "f", cluster.DefaultFile, "path to the cluster definition")
}
