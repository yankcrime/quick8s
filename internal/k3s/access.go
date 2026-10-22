package k3s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// CheckAccess verifies the generated single-context K3s kubeconfig from the
// runner, including server identity and client-certificate authentication.
func CheckAccess(ctx context.Context, kubeconfig string) error {
	var config struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
				CA     string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				Cert string `yaml:"client-certificate-data"`
				Key  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal([]byte(kubeconfig), &config); err != nil {
		return err
	}
	if len(config.Clusters) != 1 || len(config.Users) != 1 {
		return fmt.Errorf("expected the generated single-cluster K3s kubeconfig")
	}
	c, u := config.Clusters[0].Cluster, config.Users[0].User
	ca, err := base64.StdEncoding.DecodeString(c.CA)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid kubeconfig CA certificate")
	}
	cert, err := base64.StdEncoding.DecodeString(u.Cert)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(u.Key)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{pair}}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Server, "/")+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Kubernetes API is not reachable from this runner: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes API readiness returned %s", resp.Status)
	}
	return nil
}
