package k3s

import (
	"fmt"
	"strings"
)

// Installation is what, if anything, K3s is installed as on a node.
type Installation int

const (
	NotInstalled Installation = iota
	InstalledServer
	InstalledAgent
)

func (i Installation) String() string {
	switch i {
	case InstalledServer:
		return "K3s server installed"
	case InstalledAgent:
		return "K3s agent installed"
	default:
		return "K3s not installed"
	}
}

const (
	binaryPath               = "/usr/local/bin/k3s"
	uninstallScriptPath      = "/usr/local/bin/k3s-uninstall.sh"
	agentUninstallScriptPath = "/usr/local/bin/k3s-agent-uninstall.sh"
	etcdDataDir              = "/var/lib/rancher/k3s/server/db/etcd"
)

// DetectInstallation reports how K3s is installed on the node. The install
// script names its uninstall script after the service it created, so which
// one exists tells a server from an agent.
func DetectInstallation(c runner) (Installation, error) {
	out, err := c.Run(fmt.Sprintf("if test -x %s; then echo server; elif test -x %s; then echo agent; fi",
		uninstallScriptPath, agentUninstallScriptPath))
	if err != nil {
		return NotInstalled, fmt.Errorf("checking for existing k3s installation: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "server":
		return InstalledServer, nil
	case "agent":
		return InstalledAgent, nil
	default:
		return NotInstalled, nil
	}
}

// Version returns the installed K3s version, e.g. "v1.30.4+k3s1".
func Version(c runner) (string, error) {
	out, err := c.Run(binaryPath + " --version")
	if err != nil {
		return "", fmt.Errorf("checking k3s version: %w", err)
	}
	// First line: "k3s version v1.30.4+k3s1 (98262b5d)".
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "k3s" || fields[1] != "version" {
		return "", fmt.Errorf("checking k3s version: unexpected output %q", strings.TrimSpace(out))
	}
	return fields[2], nil
}

// EmbeddedEtcd reports whether a server node runs K3s's embedded etcd
// datastore, which additional control plane nodes need to join. A server
// installed without ClusterInit uses sqlite instead.
func EmbeddedEtcd(c rootRunner) (bool, error) {
	out, err := c.RunAsRoot(fmt.Sprintf("if test -d %s; then echo etcd; fi", etcdDataDir))
	if err != nil {
		return false, fmt.Errorf("checking k3s datastore: %w", err)
	}
	return strings.TrimSpace(out) == "etcd", nil
}
