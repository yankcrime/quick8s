// Package node models the remote machine quick8s bootstraps and how to reach
// it over SSH.
package node

// Target identifies a machine to bootstrap, addressed by hostname or IP,
// along with the SSH connection details needed to reach it.
type Target struct {
	Host    string // hostname or IP address
	Port    int
	User    string
	KeyPath string // path to a private key; empty means fall back to the SSH agent
}
