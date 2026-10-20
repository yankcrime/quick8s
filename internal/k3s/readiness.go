package k3s

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"quick8s/internal/shell"
)

const (
	readinessTimeout  = 3 * time.Minute
	readinessInterval = 2 * time.Second
	// These are K3s's default paths, like the config and datastore probes.
	kubeletCertPath   = "/var/lib/rancher/k3s/agent/client-kubelet.crt"
	kubeletConfigPath = "/var/lib/rancher/k3s/agent/kubelet.kubeconfig"
)

// WaitReady waits for the running service to be healthy at version. A binary
// version check alone cannot detect a service that failed to restart. Each
// remote HTTP request has its own timeout, including on OpenRC hosts.
func WaitReady(ctx context.Context, c rootRunner, installation Installation, version string, progress Progress) error {
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	return waitReady(ctx, c, installation, version, progress, readinessInterval)
}

func waitReady(ctx context.Context, c rootRunner, installation Installation, version string, progress Progress, interval time.Duration) error {
	progress.printf("Waiting for K3s readiness...")
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for K3s readiness: %w (last check: %v)", err, lastErr)
		}
		lastErr = checkReady(c, installation, version)
		if lastErr == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func checkReady(c rootRunner, installation Installation, version string) error {
	switch installation {
	case InstalledServer:
		kubectl := binaryPath + " kubectl --kubeconfig " + kubeconfigPath + " --request-timeout=5s"
		out, err := c.RunAsRoot(kubectl + " get --raw=/readyz >/dev/null && " + kubectl + " get --raw=/version")
		if err != nil {
			return err
		}
		var running struct{ GitVersion string }
		if err := json.Unmarshal([]byte(out), &running); err != nil {
			return fmt.Errorf("decoding API server version: %w", err)
		}
		if running.GitVersion != version {
			return fmt.Errorf("API server runs %s, expected %s", running.GitVersion, version)
		}
		return nil
	case InstalledAgent:
		// The certificate identifies the actual Kubernetes node name,
		// including --node-name and --with-node-id overrides.
		out, err := c.RunAsRoot("cat " + kubeletCertPath)
		if err != nil {
			return err
		}
		block, _ := pem.Decode([]byte(out))
		if block == nil {
			return fmt.Errorf("reading kubelet client certificate: no PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("reading kubelet client certificate: %w", err)
		}
		name, ok := strings.CutPrefix(cert.Subject.CommonName, "system:node:")
		if !ok || name == "" {
			return fmt.Errorf("unexpected kubelet identity %q", cert.Subject.CommonName)
		}
		// Kubernetes can retain an old Ready condition for a stopped node.
		// Require the local kubelet to answer as well as checking its status.
		out, err = c.RunAsRoot("curl -fsS --max-time 5 http://127.0.0.1:10248/healthz >/dev/null && " +
			binaryPath + " kubectl --kubeconfig " + kubeletConfigPath + " --request-timeout=5s get node " + shell.Quote(name) + " -o json")
		if err != nil {
			return err
		}
		var node struct {
			Status struct {
				NodeInfo   struct{ KubeletVersion string }
				Conditions []struct{ Type, Status string }
			}
		}
		if err := json.Unmarshal([]byte(out), &node); err != nil {
			return fmt.Errorf("decoding node status: %w", err)
		}
		if node.Status.NodeInfo.KubeletVersion != version {
			return fmt.Errorf("kubelet runs %s, expected %s", node.Status.NodeInfo.KubeletVersion, version)
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				return nil
			}
		}
		return fmt.Errorf("node %s is not Ready", name)
	default:
		return fmt.Errorf("cannot check readiness without a K3s installation")
	}
}
