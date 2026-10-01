package cli

import (
	"net"
	"os"
	"os/user"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/node"
)

// sshFlags holds the SSH connection flags shared by commands that dial a
// target node.
type sshFlags struct {
	user    string
	port    int
	keyPath string
}

// registerSSHFlags adds the shared --ssh-user/--ssh-port/--ssh-key flags to
// cmd and returns a handle for reading their values after parsing.
func registerSSHFlags(cmd *cobra.Command) *sshFlags {
	f := &sshFlags{}
	cmd.Flags().StringVar(&f.user, "ssh-user", defaultSSHUser(), "SSH user for connecting to the target node")
	cmd.Flags().IntVar(&f.port, "ssh-port", 22, "SSH port for connecting to the target node")
	cmd.Flags().StringVar(&f.keyPath, "ssh-key", "", "path to SSH private key (defaults to the SSH agent if unset)")
	return f
}

// target builds the node.Target for the given host using the parsed flags.
func (f *sshFlags) target(host string) node.Target {
	return node.Target{
		Host:    host,
		Port:    f.port,
		User:    f.user,
		KeyPath: f.keyPath,
	}
}

// specSSH builds connection settings from a cluster definition, applying
// the same defaults as the SSH flags.
func specSSH(s cluster.SSH) *sshFlags {
	f := &sshFlags{user: s.User, port: s.Port, keyPath: s.Key}
	if f.user == "" {
		f.user = defaultSSHUser()
	}
	if f.port == 0 {
		f.port = 22
	}
	return f
}

// nodeIPFor returns host if it's a literal IP address, else "". K3s's
// --node-ip flag requires a literal address - unlike --tls-san, which also
// accepts hostnames - so this guards against passing a DNS name to it.
func nodeIPFor(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	return ""
}

// defaultSSHUser mirrors ssh(1)'s own default: the local OS user, not root.
func defaultSSHUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
