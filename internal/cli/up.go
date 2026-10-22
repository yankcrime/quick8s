package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/lifecycle"
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
			"Existing hosts are checked over SSH before changes. Provider-backed clusters\n" +
			"first provision a VM and wait for guest initialization, then reconcile K3s.\n" +
			"Changes to running nodes restart K3s on them, so up asks first\n" +
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
		var managedClient *node.Client
		var managedSession *lifecycle.Session
		if spec.Managed != nil {
			session, err := lifecycle.Open(*file, spec, func(message string) { cmd.PrintErrln(message) })
			if err != nil {
				return err
			}
			defer session.Close()
			managedSession = session
			host, client, err := provision(cmd, session, ssh)
			if err != nil {
				return err
			}
			managedClient = client
			spec.ControlPlanes[0].Host = host
		}

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
			client := managedClient
			if client == nil {
				client, err = node.Dial(ssh.target(n.Host))
			}
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", n.Host, err)
			}
			clients[n.Host] = client
			state, err := probe(client)
			if err != nil {
				return fmt.Errorf("%s: %w", n.Host, err)
			}
			if managedSession != nil && managedSession.NeedsBootstrap() && state.Installation == k3s.InstalledServer {
				// Retry the installer even when an interrupted first run left
				// a matching binary but no working service. applyUpdate persists
				// its pending marker after confirmation, as for other updates.
				state.Pending = &k3s.PendingUpdate{Version: state.Version}
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

		config := func(host string) []byte {
			n, _ := spec.Node(host)
			return n.Config
		}
		if plan.InitServer {
			if err := initControlPlane(cmd, server, plan.Server, config(plan.Server), k3s.InstallOpts{
				Version: spec.KubernetesVersion,
				// A managed pool grows by changing a number, so it starts on
				// embedded etcd rather than refusing to grow later.
				ClusterInit: len(spec.ControlPlanes) > 1 || spec.Managed != nil,
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
			if err := applyUpdate(cmd, clients[u.Host], u, config(u.Host), r.options(u.Host, u.ControlPlane, u.To)); err != nil {
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
		if spec.Managed != nil {
			if err := k3s.WaitReady(cmd.Context(), server, k3s.InstalledServer, running, indented(cmd)); err != nil {
				return err
			}
		}
		for _, host := range plan.JoinControlPlanes {
			if err := addControlPlane(cmd, clients[host], host, config(host), k3s.ControlPlaneOpts{
				Version:   running,
				ServerURL: r.serverURL(),
				Token:     r.token,
			}); err != nil {
				return err
			}
		}
		for _, host := range plan.JoinWorkers {
			if err := addWorker(cmd, clients[host], host, config(host), k3s.AgentOpts{
				Version:   running,
				ServerURL: r.serverURL(),
				Token:     r.token,
			}); err != nil {
				return err
			}
		}

		relabeled, err := applyNodeMetadata(cmd, server, clients, spec.Nodes())
		if err != nil {
			return err
		}

		if plan.Empty() && !relabeled {
			cmd.PrintErrln("All nodes already match the cluster definition; nothing to do.")
		} else {
			cmd.PrintErrf("Cluster is up, running K3s %s.\n", running)
		}

		kubeconfig, err := k3s.Kubeconfig(server, plan.Server)
		if err != nil {
			return err
		}
		if spec.Managed != nil {
			cmd.PrintErrln("Checking Kubernetes API access from this runner...")
			if err := k3s.CheckAccess(cmd.Context(), kubeconfig); err != nil {
				return err
			}
			if err := managedSession.MarkReady(); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), kubeconfig)
		return err
	}

	return cmd
}

// provision creates or reuses the managed VM and connects to it once its
// guest is initialized. The deadline covers only this wait: the connection
// it returns outlives it, for the K3s rollout that follows.
func provision(cmd *cobra.Command, session *lifecycle.Session, ssh *sshFlags) (string, *node.Client, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
	defer cancel()
	host, err := session.Ensure(ctx)
	if err != nil {
		return "", nil, err
	}
	client, err := lifecycle.WaitGuest(ctx, ssh.target(host), func(message string) { cmd.PrintErrln(message) })
	return host, client, err
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

// applyNodeMetadata converges the labels and taints of nodes whose pools
// declare them, through the API, once all nodes have joined. Other nodes
// are left alone, with no API calls. Unlike K3s config changes, this needs
// no restart, so it isn't part of the confirmed plan.
func applyNodeMetadata(cmd *cobra.Command, server *node.Client, clients map[string]*node.Client, nodes []cluster.Node) (bool, error) {
	changed := false
	for _, n := range nodes {
		if n.Metadata == nil {
			continue
		}
		name, updated, err := k3s.ApplyNodeMetadata(cmd.Context(), clients[n.Host], server, *n.Metadata)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", n.Host, err)
		}
		if updated {
			cmd.PrintErrf("Updated labels and taints on %s (node %s).\n", n.Host, name)
			changed = true
		}
	}
	return changed, nil
}

// describeUpdate summarizes an in-place change for confirmation.
func describeUpdate(spec *cluster.Spec, u cluster.Update) string {
	n, _ := spec.Node(u.Host)
	var changes []string
	if u.Upgrade() {
		changes = append(changes, fmt.Sprintf("upgrade K3s %s -> %s", u.From, u.To))
	}
	switch {
	case u.Config && n.Config == nil:
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
