package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadExample(t *testing.T) {
	spec, err := Load("../../examples/quick8s.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if spec.KubernetesVersion != "v1.33.4+k3s1" || spec.SSH.User != "ubuntu" || spec.SSH.Port != 0 {
		t.Fatalf("unexpected settings: %+v", spec)
	}
	want := []Node{{"192.168.1.10"}, {"192.168.1.11"}, {"192.168.1.12"}, {"192.168.1.20"}, {"192.168.1.21"}}
	if !reflect.DeepEqual(spec.Nodes(), want) {
		t.Fatalf("nodes = %v", spec.Nodes())
	}
	if !strings.Contains(string(spec.ServerConfig), "disable:\n    - traefik\n") || strings.Contains(string(spec.ServerConfig), "node-label") {
		t.Fatalf("server config = %q", spec.ServerConfig)
	}
	if string(spec.AgentConfig) != "node-label:\n    - tier=general\n" {
		t.Fatalf("agent config = %q", spec.AgentConfig)
	}
}

func TestParseK3sConfigPassthrough(t *testing.T) {
	spec, err := parse([]byte(`
cluster:
  nodes:
    controlPlane: [a]
k3s:
  server:
    write-kubeconfig-mode: "0644"
    cluster-init: true
    kubelet-arg: [max-pods=200]
`), ".")
	if err != nil {
		t.Fatal(err)
	}
	// Key order and scalar styles survive, so K3s sees what was written.
	want := "write-kubeconfig-mode: \"0644\"\ncluster-init: true\nkubelet-arg: [max-pods=200]\n"
	if string(spec.ServerConfig) != want {
		t.Fatalf("got %q, want %q", spec.ServerConfig, want)
	}
	if spec.AgentConfig != nil {
		t.Fatalf("absent agent config should be nil, got %q", spec.AgentConfig)
	}
}

func TestParseSSHKeyPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"~/.ssh/id":    filepath.Join(home, ".ssh/id"),
		"keys/id":      "/clusters/prod/keys/id",
		"/abs/path/id": "/abs/path/id",
	} {
		spec, err := parse([]byte("cluster:\n  nodes: {controlPlane: [a]}\n  ssh: {key: "+key+"}\n"), "/clusters/prod")
		if err != nil || spec.SSH.Key != want {
			t.Fatalf("key %q resolved to %q (%v), want %q", key, spec.SSH.Key, err, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, tt := range []struct{ name, yaml, want string }{
		{"empty", "", "empty"},
		{"no control plane", "cluster: {nodes: {worker: [a]}}", "cluster.nodes.controlPlane is required"},
		{"not under cluster", "nodes: {controlPlane: [a]}", "field nodes not found"},
		{"old workers key", "cluster: {nodes: {controlPlane: [a], workers: [b]}}", "field workers not found"},
		{"k3s under cluster", "cluster: {nodes: {controlPlane: [a]}, k3s: {server: {}}}", "field k3s not found"},
		{"old object form", "cluster: {nodes: {controlPlane: [{host: a}]}}", "cannot unmarshal"},
		{"missing host", "cluster: {nodes: {controlPlane: ['']}}", "cluster.nodes.controlPlane[0]: hostname or IP is required"},
		{"duplicate host", "cluster: {nodes: {controlPlane: [a], worker: [b, a]}}", "cluster.nodes.worker[1]: a is already listed under cluster.nodes.controlPlane"},
		{"bare kubernetes version", "cluster: {kubernetesVersion: v1.33.4, nodes: {controlPlane: [a]}}", "full release tag"},
		{"bad port", "cluster: {ssh: {port: 70000}, nodes: {controlPlane: [a]}}", "cluster.ssh.port"},
		{"k3s list", "cluster: {nodes: {controlPlane: [a]}}\nk3s: {server: [a]}", "k3s.server must be a mapping"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml), ".")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingDefaultFile(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := Load(DefaultFile)
	if err == nil || !strings.Contains(err.Error(), "--file") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
