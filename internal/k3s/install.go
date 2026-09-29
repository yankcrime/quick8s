// Package k3s orchestrates installing K3s on a remote node over an already
// established SSH connection.
package k3s

import (
	"fmt"

	"quick8s/internal/node"
)

// InstallOpts controls how the K3s install script is invoked on the remote node.
type InstallOpts struct {
	// Version is the K3s channel/version to install, e.g. "v1.30.4+k3s1".
	// Empty means "latest stable", per the upstream install script's default.
	Version string
	// ClusterInit enables K3s's embedded etcd datastore instead of the
	// default sqlite, required if any additional control plane nodes will
	// join this one (see JoinControlPlane). Leave false for a single
	// control plane node.
	ClusterInit bool
	// TLSSan is added as a certificate SAN, so the server's cert is valid
	// for the address quick8s (and later, kubectl) actually uses to reach
	// it. Without this, K3s's auto-detected node-ip may end up as the only
	// SAN, and any client connecting via a different address - including
	// the kubeconfig quick8s hands back - fails TLS verification. Should be
	// the same host used to dial this node.
	TLSSan string
	// NodeIP pins the address K3s advertises for this node (node-ip and,
	// for a control plane, its etcd peer URL) instead of letting K3s
	// auto-detect it. On a multi-homed host, auto-detection can grab a
	// transient/wrong interface address during a cold-boot DHCP race,
	// which then gets baked into etcd's member registry and never
	// self-corrects. Should be the same host used to dial this node -
	// same value as TLSSan.
	NodeIP string
}

// Install downloads and runs the official K3s install script on the target
// node via the given SSH client.
func Install(c *node.Client, opts InstallOpts) error {
	script := "curl -sfL https://get.k3s.io |"
	if opts.Version != "" {
		script += fmt.Sprintf(" INSTALL_K3S_VERSION=%q", opts.Version)
	}
	script += " sh -s - server"
	if opts.ClusterInit {
		script += " --cluster-init"
	}
	if opts.TLSSan != "" {
		script += fmt.Sprintf(" --tls-san %q", opts.TLSSan)
	}
	if opts.NodeIP != "" {
		script += fmt.Sprintf(" --node-ip %q", opts.NodeIP)
	}

	out, err := c.Run(script)
	if err != nil {
		return fmt.Errorf("installing k3s: %w\n%s", err, out)
	}
	return nil
}
