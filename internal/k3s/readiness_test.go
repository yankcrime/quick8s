package k3s

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func kubeletCertificate(t *testing.T, name string) string {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestWaitReadyServer(t *testing.T) {
	const version = "v1.34.1+k3s1"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attempts := 0
	c := remoteStub{runRoot: func(string) (string, error) {
		attempts++
		switch attempts {
		case 1:
			return "", errors.New("API server is unavailable")
		case 2:
			return `{"gitVersion":"v1.33.4+k3s1"}`, nil
		default:
			return `{"gitVersion":"` + version + `"}`, nil
		}
	}}
	if err := waitReady(ctx, c, InstalledServer, version, nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("returned before running target version: %d attempts", attempts)
	}
}

func TestWaitReadyTimeoutAndCancellation(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		err  error
	}{
		{name: "unavailable", err: errors.New("connection refused")},
		{name: "old version", out: `{"gitVersion":"v1.33.4+k3s1"}`},
		{name: "malformed response", out: "not JSON"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			c := remoteStub{runRoot: func(string) (string, error) { return tt.out, tt.err }}
			if err := waitReady(ctx, c, InstalledServer, "v1.34.1+k3s1", nil, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want readiness timeout", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := remoteStub{runRoot: func(string) (string, error) {
		t.Fatal("ran remote command after cancellation")
		return "", nil
	}}
	if err := WaitReady(ctx, c, InstalledServer, "v1.34.1+k3s1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestAgentReadiness(t *testing.T) {
	cert := kubeletCertificate(t, "system:node:custom-worker-abc123")
	for _, tt := range []struct {
		name, version, condition string
		healthErr                error
		wantReady                bool
	}{
		{name: "ready", version: "v1.34.1+k3s1", condition: "True", wantReady: true},
		{name: "old kubelet", version: "v1.33.4+k3s1", condition: "True"},
		{name: "not ready", version: "v1.34.1+k3s1", condition: "False"},
		{name: "missing ready condition", version: "v1.34.1+k3s1"},
		{name: "stopped local kubelet with stale Ready", version: "v1.34.1+k3s1", condition: "True", healthErr: errors.New("connection refused")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := remoteStub{runRoot: func(cmd string) (string, error) {
				if cmd == "cat "+kubeletCertPath {
					return cert, nil
				}
				if !strings.Contains(cmd, "custom-worker-abc123") || !strings.Contains(cmd, "/healthz") {
					t.Fatalf("did not check the local kubelet and its actual node identity: %s", cmd)
				}
				return `{"status":{"nodeInfo":{"kubeletVersion":"` + tt.version + `"},"conditions":[{"type":"Ready","status":"` + tt.condition + `"}]}}`, tt.healthErr
			}}
			err := checkReady(c, InstalledAgent, "v1.34.1+k3s1")
			if (err == nil) != tt.wantReady {
				t.Fatalf("readiness = %v, want ready %v", err, tt.wantReady)
			}
		})
	}
}

func TestAgentReadinessRejectsInvalidIdentity(t *testing.T) {
	for _, cert := range []string{"not a certificate", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")})), kubeletCertificate(t, "not-a-node")} {
		c := remoteStub{runRoot: func(string) (string, error) { return cert, nil }}
		if err := checkReady(c, InstalledAgent, "v1.34.1+k3s1"); err == nil {
			t.Fatal("accepted an invalid kubelet certificate")
		}
	}
}
