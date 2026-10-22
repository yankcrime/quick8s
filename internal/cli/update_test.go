package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"quick8s/internal/node/nodetest"
)

// updateSSH simulates remote commands without running shell commands or K3s.
func updateSSH(t *testing.T, run func(string, io.Reader) (string, error)) int {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodetest.Agent(t, key)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "sessions only")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					// Commands on each client run sequentially.
					func() {
						defer channel.Close()
						for req := range requests {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
								req.Reply(false, nil)
								return
							}
							req.Reply(true, nil)
							out, err := run(payload.Command, channel)
							io.WriteString(channel, out)
							status := uint32(0)
							if err != nil {
								fmt.Fprintln(channel.Stderr(), err)
								status = 1
							}
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
		connections.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port
}

// fakeNode answers the commands up uses to label and taint a node, keeping
// what it's patched with so a later run sees its own changes.
type fakeNode struct {
	cert   string
	object map[string]any
	calls  int
}

func newFakeNode(t *testing.T, name string) *fakeNode {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "system:node:" + name}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeNode{
		cert:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		object: map[string]any{"metadata": map[string]any{"name": name, "resourceVersion": "1"}},
	}
}

func (n *fakeNode) handle(cmd string) (string, bool, error) {
	if strings.Contains(cmd, "client-kubelet.crt") || strings.Contains(cmd, " get node ") || strings.Contains(cmd, " patch node ") {
		n.calls++
	}
	switch {
	case strings.Contains(cmd, "cat /var/lib/rancher/k3s/agent/client-kubelet.crt"):
		return n.cert, true, nil
	case strings.Contains(cmd, " get node "):
		out, err := json.Marshal(n.object)
		return string(out), true, err
	case strings.Contains(cmd, " patch node "):
		// The payload is shell-quoted inside the root wrapper; JSON without
		// single quotes survives that intact, so decode the first value.
		_, payload, _ := strings.Cut(cmd, " -p ")
		var patch map[string]any
		if err := json.NewDecoder(strings.NewReader(payload[strings.Index(payload, "{"):])).Decode(&patch); err != nil {
			return "", true, err
		}
		mergePatch(n.object, patch)
		return "", true, nil
	}
	return "", false, nil
}

// mergePatch applies a JSON merge patch (RFC 7386) in place.
func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		switch v := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			child, ok := dst[k].(map[string]any)
			if !ok {
				child = map[string]any{}
				dst[k] = child
			}
			mergePatch(child, v)
		default:
			dst[k] = v
		}
	}
}

func TestCommandsRetryPendingUpdates(t *testing.T) {
	const old, target = "v1.33.4+k3s1", "v1.34.1+k3s1"
	for _, command := range []string{"up", "upgrade"} {
		for _, failure := range []string{"restart", "installer", "readiness"} {
			if command == "upgrade" && failure == "restart" {
				continue // Config-only restarts belong to up.
			}
			t.Run(command+"/"+failure, func(t *testing.T) {
				var mu sync.Mutex
				version, diskConfig := old, ""
				var pending *string
				attempts, readinessChecks := 0, 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				kube := newFakeNode(t, "test-node")
				port := updateSSH(t, func(cmd string, stdin io.Reader) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					if out, ok, err := kube.handle(cmd); ok {
						return out, err
					}
					switch {
					case strings.Contains(cmd, "k3s-uninstall.sh"):
						return "server\n", nil
					case strings.Contains(cmd, "--version"):
						return "k3s version " + version + " (test)\n", nil
					case strings.Contains(cmd, "echo present") && strings.Contains(cmd, ".quick8s-update-pending"):
						if pending != nil {
							return "present\n" + *pending, nil
						}
						return "", nil
					case strings.Contains(cmd, ".quick8s-update-pending") && strings.Contains(cmd, "mv "):
						v := ""
						if strings.Contains(cmd, target) {
							v = target
						}
						pending = &v
						return "", nil
					case strings.Contains(cmd, ".quick8s-update-pending") && strings.Contains(cmd, "rm -f"):
						if readinessChecks == 0 {
							return "", errors.New("cleared marker before checking readiness")
						}
						pending = nil
						return "", nil
					case strings.Contains(cmd, "echo present"):
						if diskConfig != "" {
							return "present\n" + diskConfig, nil
						}
						return "", nil
					case strings.Contains(cmd, "tee "):
						if pending == nil {
							return "", errors.New("config changed before recording intent")
						}
						data, err := io.ReadAll(stdin)
						diskConfig = string(data)
						return "", err
					case strings.Contains(cmd, "systemctl restart"), strings.Contains(cmd, "https://get.k3s.io"):
						if pending == nil {
							return "", errors.New("service changed before recording intent")
						}
						attempts++
						if failure != "restart" {
							version = target // Binary replacement precedes service startup.
							if !strings.Contains(cmd, "INSTALL_K3S_FORCE_RESTART=true") {
								return "", errors.New("installer retry must force a restart")
							}
						}
						if attempts == 1 && failure != "readiness" {
							return "", errors.New("simulated update failure")
						}
						return "", nil
					case strings.Contains(cmd, "--raw=/readyz"):
						readinessChecks++
						if failure == "readiness" && attempts == 1 {
							cancel()
							return "", errors.New("service not ready")
						}
						return `{"gitVersion":"` + version + `"}`, nil
					case strings.Contains(cmd, "k3s.yaml"):
						return "apiVersion: v1\nclusters: []\n", nil
					case strings.Contains(cmd, "db/etcd"):
						return "", nil
					default:
						return "", fmt.Errorf("unexpected command: %s", cmd)
					}
				})
				desired := target
				if failure == "restart" {
					desired = old
				}
				specPath := filepath.Join(t.TempDir(), "quick8s.yaml")
				spec := fmt.Sprintf("cluster:\n  kubernetesVersion: %s\n  ssh:\n    user: test\n    port: %d\n  nodes:\n    controlPlane: [{name: servers, hosts: [127.0.0.1]}]\nk3s:\n  server:\n    disable: [traefik]\n", desired, port)
				if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
					t.Fatal(err)
				}
				run := func(ctx context.Context) (string, error) {
					cmd := NewRootCmd()
					cmd.SetContext(ctx)
					var stderr bytes.Buffer
					cmd.SetErr(&stderr)
					cmd.SetOut(io.Discard)
					args := []string{"up", "-y", "-f", specPath}
					if command == "upgrade" {
						args = []string{"upgrade", "127.0.0.1", "-y", "--k3s-version", target, "--ssh-user", "test", "--ssh-port", fmt.Sprint(port)}
					}
					cmd.SetArgs(args)
					err := cmd.Execute()
					return stderr.String(), err
				}
				if out, err := run(ctx); err == nil {
					t.Fatalf("first update unexpectedly succeeded:\n%s", out)
				}
				mu.Lock()
				hasPending := pending != nil
				mu.Unlock()
				if !hasPending {
					t.Fatal("failed update lost its pending marker")
				}
				if out, err := run(context.Background()); err != nil {
					t.Fatalf("retry failed: %v\n%s", err, out)
				}
				if out, err := run(context.Background()); err != nil || !strings.Contains(out, "nothing to do") {
					t.Fatalf("completed update was not a no-op: %v\n%s", err, out)
				}
				mu.Lock()
				defer mu.Unlock()
				if pending != nil || attempts != 2 || readinessChecks == 0 {
					t.Fatalf("pending=%v, attempts=%d, readiness checks=%d", pending, attempts, readinessChecks)
				}
				// Without labels or taints, up never calls the Kubernetes API.
				if kube.calls != 0 {
					t.Fatalf("made %d Kubernetes API calls for a pool without labels or taints", kube.calls)
				}
			})
		}
	}
}

func TestReadinessFailureStopsRollout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	installs, checks := 0, 0
	port := updateSSH(t, func(cmd string, _ io.Reader) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(cmd, "k3s-uninstall.sh"):
			return "server\n", nil
		case strings.Contains(cmd, "--version"):
			return "k3s version v1.33.4+k3s1 (test)\n", nil
		case strings.Contains(cmd, "echo present"), strings.Contains(cmd, "db/etcd"):
			return "", nil
		case strings.Contains(cmd, "server/node-token"):
			return "test-token\n", nil
		case strings.Contains(cmd, ".quick8s-update-pending") && strings.Contains(cmd, "mv "):
			return "", nil
		case strings.Contains(cmd, "https://get.k3s.io"):
			installs++
			return "", nil // Like OpenRC, starting successfully says nothing about readiness.
		case strings.Contains(cmd, "--raw=/readyz"):
			checks++
			cancel()
			return "", errors.New("API server failed to become ready")
		default:
			return "", fmt.Errorf("unexpected command after failure: %s", cmd)
		}
	})
	cmd := NewRootCmd()
	cmd.SetContext(ctx)
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"upgrade", "--control-plane", "127.0.0.1,localhost", "-y", "--k3s-version", "v1.34.1+k3s1", "--ssh-user", "test", "--ssh-port", fmt.Sprint(port)})
	if err := cmd.Execute(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected rollout to fail on readiness, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if installs != 1 || checks != 1 {
		t.Fatalf("continued to another node: installs=%d, checks=%d", installs, checks)
	}
}
