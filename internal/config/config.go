// Package config holds the resolved options for a quick8s run, whether
// they come from CLI flags or (in future) a saved cluster definition file.
package config

// Bootstrap holds the resolved options for bootstrapping a single K3s node.
type Bootstrap struct {
	Target     string // hostname or IP of the node
	SSHUser    string
	SSHPort    int
	SSHKeyPath string
	K3sVersion string // e.g. "v1.30.4+k3s1"; empty means "latest"
}
