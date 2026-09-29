package k3s

import (
	"fmt"

	"quick8s/internal/node"
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
	// TLSSan is added as a certificate SAN for this node's own server cert
	// (each control plane node runs its own apiserver). Should be the host
	// used to dial this specific node - see InstallOpts.TLSSan.
	TLSSan string
	// NodeIP pins this node's own advertised address - see InstallOpts.NodeIP.
	NodeIP string
}

// JoinControlPlane installs K3s in server mode on the target node, joining
// it to the cluster at ServerURL as an additional control plane / etcd
// member. The initiating control plane must have been installed with
// InstallOpts.ClusterInit for this to work.
func JoinControlPlane(c *node.Client, opts ControlPlaneOpts) error {
	script := fmt.Sprintf("curl -sfL https://get.k3s.io | K3S_TOKEN=%q", opts.Token)
	if opts.Version != "" {
		script += fmt.Sprintf(" INSTALL_K3S_VERSION=%q", opts.Version)
	}
	script += fmt.Sprintf(" sh -s - server --server %q", opts.ServerURL)
	if opts.TLSSan != "" {
		script += fmt.Sprintf(" --tls-san %q", opts.TLSSan)
	}
	if opts.NodeIP != "" {
		script += fmt.Sprintf(" --node-ip %q", opts.NodeIP)
	}

	out, err := c.Run(script)
	if err != nil {
		return fmt.Errorf("joining control plane: %w\n%s", err, out)
	}
	return nil
}
