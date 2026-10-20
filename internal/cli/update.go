package cli

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/k3s"
	"quick8s/internal/node"
)

// reinstall holds what re-running the K3s installer on any node of an
// existing cluster needs, to repeat the options quick8s installs that node
// with. The installer rewrites the K3s service from its arguments on every
// run, so an upgrade has to pass them all again.
type reinstall struct {
	// server is the control plane other nodes join. It's reinstalled as
	// the cluster's initializing server, and the rest point at it.
	server string
	// clusterInit is whether server runs embedded etcd.
	clusterInit bool
	// token is the cluster join token, needed by every node but server.
	token string
}

// newReinstall inspects server, and fetches the join token from it when
// nodes other than server will be reinstalled.
func newReinstall(cmd *cobra.Command, client *node.Client, server string, needToken bool) (reinstall, error) {
	r := reinstall{server: server}
	var err error
	if r.clusterInit, err = k3s.EmbeddedEtcd(client); err != nil {
		return r, fmt.Errorf("control plane %s: %w", server, err)
	}
	if needToken {
		cmd.PrintErrf("Fetching node token from %s...\n", server)
		if r.token, err = k3s.NodeToken(client); err != nil {
			return r, err
		}
	}
	return r, nil
}

// serverURL is the API address nodes join.
func (r reinstall) serverURL() string {
	return "https://" + net.JoinHostPort(r.server, "6443")
}

// options returns how to install host in its role at version, the same
// way initControlPlane, addControlPlane and addWorker do.
func (r reinstall) options(host string, controlPlane bool, version string) k3s.Options {
	switch {
	case !controlPlane:
		return k3s.AgentOpts{Version: version, ServerURL: r.serverURL(), Token: r.token, NodeIP: nodeIPFor(host)}
	case host == r.server:
		return k3s.InstallOpts{Version: version, ClusterInit: r.clusterInit, TLSSAN: host, NodeIP: nodeIPFor(host)}
	default:
		return k3s.ControlPlaneOpts{Version: version, ServerURL: r.serverURL(), Token: r.token, TLSSAN: host, NodeIP: nodeIPFor(host)}
	}
}

// applyUpdate keeps a node's update pending until the running service is
// ready. Both commands use this path, so a failed or interrupted update is
// retried even if it already replaced the config or binary on disk.
func applyUpdate(cmd *cobra.Command, client *node.Client, u cluster.Update, config []byte, opts k3s.Options) error {
	installation := k3s.InstalledAgent
	if u.ControlPlane {
		installation = k3s.InstalledServer
	}
	expected := u.To
	if expected == "" {
		var err error
		expected, err = k3s.Version(client)
		if err != nil {
			return fmt.Errorf("%s: %w", u.Host, err)
		}
	}
	if err := k3s.BeginUpdate(client, u.To); err != nil {
		return fmt.Errorf("%s: %w", u.Host, err)
	}
	if u.Config {
		if err := replaceConfig(cmd, client, u.Host, config); err != nil {
			return err
		}
	}
	title := fmt.Sprintf("Restarting K3s on %s...", u.Host)
	if u.Upgrade() {
		title = fmt.Sprintf("Upgrading K3s on %s from %s to %s...", u.Host, u.From, u.To)
	}
	if err := slowStep(cmd, title, func(progress k3s.Progress) error {
		var err error
		if u.Upgrade() {
			err = k3s.Upgrade(client, opts, progress)
		} else {
			err = k3s.Restart(client, installation)
		}
		if err != nil {
			return err
		}
		return k3s.WaitReady(cmd.Context(), client, installation, expected, progress)
	}); err != nil {
		return fmt.Errorf("%s: %w", u.Host, err)
	}
	if err := k3s.FinishUpdate(client); err != nil {
		return fmt.Errorf("%s: %w", u.Host, err)
	}
	return nil
}

// replaceConfig makes an already-connected node's K3s config file hold
// config, or removes it when config is nil. K3s only reads it on start.
func replaceConfig(cmd *cobra.Command, client *node.Client, host string, config []byte) error {
	if config == nil {
		cmd.PrintErrf("Removing K3s config from %s...\n", host)
		if err := k3s.RemoveConfig(client); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		return nil
	}
	cmd.PrintErrf("Pushing K3s config to %s...\n", host)
	if err := k3s.PushConfig(client, config); err != nil {
		return fmt.Errorf("%s: %w", host, err)
	}
	return nil
}
