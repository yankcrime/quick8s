// Package node models the remote machine quick8s bootstraps and how to reach
// it over SSH.
package node

// Target identifies a machine to bootstrap, addressed by hostname or IP,
// along with the SSH connection details needed to reach it.
type Target struct {
	Host string // hostname or IP address
	Port int
	User string
	// KeyPath is a private key offered after the SSH agent's keys, or
	// empty to use the agent alone.
	KeyPath string
}
