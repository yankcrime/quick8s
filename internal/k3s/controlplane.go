package k3s

import (
	"fmt"
)

// ControlPlaneOpts controls how an additional control plane node joins an
// existing K3s cluster's embedded etcd.
type ControlPlaneOpts struct {
	// Version should match the initiating control plane's K3s version.
	Version string
	// ServerURL is the initiating control plane's API address, e.g.
	// "https://<host>:6443".
	ServerURL string
	// Token is the cluster join token, from NodeToken.
	Token string
	// TLSSAN is added as a certificate SAN for this node's own server cert
	// (each control plane node runs its own apiserver). Should be the host
	// used to dial this specific node - see InstallOpts.TLSSAN.
	TLSSAN string
	// NodeIP pins this node's own advertised address - see InstallOpts.NodeIP.
	NodeIP string
}

// JoinControlPlane installs K3s in server mode on the target node, joining
// it to the cluster at ServerURL as an additional control plane / etcd
// member. The initiating control plane must have been installed with
// InstallOpts.ClusterInit for this to work.
func JoinControlPlane(c rootStreamer, opts ControlPlaneOpts, progress Progress) error {
	env := []string{"K3S_TOKEN=" + opts.Token}
	args := []string{"server", "--server", opts.ServerURL}
	if opts.TLSSAN != "" {
		args = append(args, "--tls-san", opts.TLSSAN)
	}
	if opts.NodeIP != "" {
		args = append(args, "--node-ip", opts.NodeIP)
	}

	if err := runInstall(c, opts.Version, env, args, progress); err != nil {
		return fmt.Errorf("joining control plane: %w", err)
	}
	return nil
}
