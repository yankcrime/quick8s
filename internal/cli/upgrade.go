package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

func newUpgradeCmd() *cobra.Command {
	var (
		k3sVersion    string
		yes           bool
		workers       []string
		controlPlanes []string
	)

	cmd := &cobra.Command{
		Use:   "upgrade [target]",
		Short: "Upgrade K3s on an existing cluster",
		Long: "Upgrade K3s on the nodes of an existing cluster over SSH, e.g. one created\n" +
			"with bootstrap. Nodes are named as for bootstrap: a single node as the\n" +
			"positional [target], or else every control plane with --control-plane (the\n" +
			"first is the one the others join) and workers with --worker.\n\n" +
			"Every node is checked and the upgrade validated before anything changes:\n" +
			"Kubernetes supports neither downgrades nor skipping a minor version. Nodes\n" +
			"are then upgraded one at a time, control planes first, and nodes already on\n" +
			"--k3s-version are left alone. For a cluster defined in quick8s.yaml, change\n" +
			"cluster.kubernetesVersion and run up instead.",
		Args: cobra.MaximumNArgs(1),
	}

	ssh := registerSSHFlags(cmd)
	cmd.Flags().StringVar(&k3sVersion, "k3s-version", "", "K3s release to upgrade to, e.g. v1.34.1+k3s1 (required)")
	cmd.Flags().StringSliceVar(&controlPlanes, "control-plane", nil, "hostname or IP of a control plane node; the first one given is the one the others join (repeatable, or comma-separated)")
	cmd.Flags().StringSliceVar(&workers, "worker", nil, "hostname or IP of a worker node (repeatable, or comma-separated)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	_ = cmd.MarkFlagRequired("k3s-version")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		controlPlanes, err := resolveControlPlanes(args, controlPlanes, workers)
		if err != nil {
			return err
		}
		if _, err := k3s.ParseRelease(k3sVersion); err != nil {
			return fmt.Errorf("--k3s-version: %w", err)
		}
		// Arguments are valid by now; failures from here on aren't usage errors.
		cmd.SilenceUsage = true

		// Check every node before upgrading any, holding the connections
		// open for the upgrades.
		var upgrades []cluster.Update
		clients := map[string]*node.Client{}
		defer func() {
			for _, c := range clients {
				c.Close()
			}
		}()
		check := func(host string, controlPlane bool) error {
			client, err := node.Dial(ssh.target(host))
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", host, err)
			}
			clients[host] = client
			if err := checkRole(client, host, controlPlane); err != nil {
				return err
			}
			from, err := k3s.Version(client)
			if err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
			needed, err := k3s.CheckUpgrade(from, k3sVersion)
			if err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
			pending, err := k3s.ReadPendingUpdate(client)
			if err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
			if !needed && pending == nil {
				cmd.PrintErrf("  %s: already running %s\n", host, from)
				return nil
			}
			cmd.PrintErrf("  %s: %s -> %s\n", host, from, k3sVersion)
			upgrades = append(upgrades, cluster.Update{Host: host, ControlPlane: controlPlane, From: from, To: k3sVersion})
			return nil
		}
		cmd.PrintErrln("Checking nodes...")
		for _, host := range controlPlanes {
			if err := check(host, true); err != nil {
				return err
			}
		}
		for _, host := range workers {
			if err := check(host, false); err != nil {
				return err
			}
		}

		if len(upgrades) == 0 {
			cmd.PrintErrf("All nodes already run K3s %s; nothing to do.\n", k3sVersion)
			return nil
		}
		if !yes {
			ok, err := confirm(cmd, fmt.Sprintf("This will upgrade K3s on %d node(s), restarting K3s on each in turn. Continue?", len(upgrades)))
			if err != nil {
				return err
			}
			if !ok {
				cmd.PrintErrln("Aborted.")
				return nil
			}
		}

		server := controlPlanes[0]
		needToken := false
		for _, u := range upgrades {
			needToken = needToken || u.Host != server
		}
		r, err := newReinstall(cmd, clients[server], server, needToken)
		if err != nil {
			return err
		}
		for _, u := range upgrades {
			if err := applyUpdate(cmd, clients[u.Host], u, nil, r.options(u.Host, u.ControlPlane, k3sVersion)); err != nil {
				return err
			}
		}

		cmd.PrintErrf("Cluster upgraded to K3s %s.\n", k3sVersion)
		return nil
	}

	return cmd
}

// checkRole confirms K3s is installed on an already-connected node in the
// role it was named with.
func checkRole(client *node.Client, host string, controlPlane bool) error {
	installation, err := k3s.DetectInstallation(client)
	if err != nil {
		return fmt.Errorf("%s: %w", host, err)
	}
	switch {
	case installation == k3s.NotInstalled:
		return fmt.Errorf("%s doesn't have K3s installed; upgrade only changes existing nodes", host)
	case controlPlane && installation != k3s.InstalledServer:
		return fmt.Errorf("%s was given as a control plane, but K3s is installed there as an agent (worker)", host)
	case !controlPlane && installation != k3s.InstalledAgent:
		return fmt.Errorf("%s was given as a worker, but K3s is installed there as a server (control plane)", host)
	}
	return nil
}
