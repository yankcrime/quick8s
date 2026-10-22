package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"quick8s/internal/k3s"
)

func TestLoadExample(t *testing.T) {
	spec, err := Load("../../examples/quick8s.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if spec.KubernetesVersion != "v1.33.4+k3s1" || spec.SSH.User != "ubuntu" || spec.SSH.Port != 0 {
		t.Fatalf("unexpected settings: %+v", spec)
	}
	var hosts, pools []string
	for _, n := range spec.Nodes() {
		hosts, pools = append(hosts, n.Host), append(pools, n.Pool)
	}
	if want := []string{"192.168.1.10", "192.168.1.11", "192.168.1.12", "192.168.1.20", "192.168.1.21", "192.168.1.30"}; !reflect.DeepEqual(hosts, want) {
		t.Fatalf("hosts = %v", hosts)
	}
	if want := []string{"servers", "servers", "servers", "general", "general", "gpu"}; !reflect.DeepEqual(pools, want) {
		t.Fatalf("pools = %v", pools)
	}
	server := string(spec.ControlPlanes[0].Config)
	if !strings.Contains(server, "disable:\n    - traefik\n") || strings.Contains(server, "kubelet-arg") {
		t.Fatalf("server config = %q", server)
	}
	if got := string(spec.Workers[0].Config); got != "kubelet-arg: [max-pods=110]\n" {
		t.Fatalf("general config = %q", got)
	}
	gpu := spec.Workers[2]
	if got := string(gpu.Config); got != "kubelet-arg: [max-pods=50]\n" {
		t.Fatalf("gpu config = %q", got)
	}
	want := &k3s.NodeMetadata{
		Labels: map[string]string{"accelerator": "nvidia"},
		Taints: []k3s.Taint{{Key: "nvidia.com/gpu", Value: "true", Effect: "NoSchedule"}},
	}
	if !reflect.DeepEqual(gpu.Metadata, want) {
		t.Fatalf("gpu metadata = %+v", gpu.Metadata)
	}
	// A pool without labels or taints is left alone entirely.
	if got := spec.ControlPlanes[0].Metadata; got != nil {
		t.Fatalf("server metadata = %+v", got)
	}
}

// Declaring labels or taints, even empty, opts a pool in, so removing its
// last label still clears what quick8s set; leaving both out opts it out.
func TestParseMetadataOptIn(t *testing.T) {
	for in, want := range map[string]*k3s.NodeMetadata{
		"{name: a, hosts: [a]}":                    nil,
		"{name: a, hosts: [a], labels: }":          nil,
		"{name: a, hosts: [a], labels: {}}":        {Labels: map[string]string{}},
		"{name: a, hosts: [a], taints: []}":        {Labels: map[string]string{}},
		"{name: a, hosts: [a], labels: {tier: x}}": {Labels: map[string]string{"tier": "x"}},
	} {
		spec, err := parse([]byte("cluster: {nodes: {controlPlane: ["+in+"]}}"), ".")
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := spec.ControlPlanes[0].Metadata; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: metadata = %+v, want %+v", in, got, want)
		}
	}
}

func TestParseK3sConfigPassthrough(t *testing.T) {
	spec, err := parse([]byte(`
cluster:
  nodes:
    controlPlane:
      - {name: a, hosts: [a]}
      - name: b
        hosts: [b]
        k3s:
          kubelet-arg: [max-pods=50]
          node-taint: [x=y:NoSchedule]
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
	if got := string(spec.ControlPlanes[0].Config); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A pool's keys replace the role's in place; new ones follow.
	want = "write-kubeconfig-mode: \"0644\"\ncluster-init: true\nkubelet-arg: [max-pods=50]\nnode-taint: ['x=y:NoSchedule']\n"
	if got := string(spec.ControlPlanes[1].Config); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	spec, err = parse([]byte("cluster: {nodes: {controlPlane: [{name: a, hosts: [a]}], worker: [{name: w, hosts: [w]}]}}"), ".")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ControlPlanes[0].Config != nil || spec.Workers[0].Config != nil {
		t.Fatalf("absent config should be nil, got %q and %q", spec.ControlPlanes[0].Config, spec.Workers[0].Config)
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
		spec, err := parse([]byte("cluster:\n  nodes: {controlPlane: [{name: a, hosts: [a]}]}\n  ssh: {key: "+key+"}\n"), "/clusters/prod")
		if err != nil || spec.SSH.Key != want {
			t.Fatalf("key %q resolved to %q (%v), want %q", key, spec.SSH.Key, err, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	const cp = "controlPlane: [{name: a, hosts: [a]}]"
	for _, tt := range []struct{ name, yaml, want string }{
		{"empty", "", "empty"},
		{"no control plane", "cluster: {nodes: {worker: [{name: w, hosts: [a]}]}}", "cluster.nodes.controlPlane is required"},
		{"not under cluster", "nodes: {" + cp + "}", "field nodes not found"},
		{"old workers key", "cluster: {nodes: {" + cp + ", workers: []}}", "field workers not found"},
		{"k3s under cluster", "cluster: {nodes: {" + cp + "}, k3s: {server: {}}}", "field k3s not found"},
		{"bare address", "cluster: {nodes: {controlPlane: [a]}}", "cannot unmarshal"},
		{"unnamed pool", "cluster: {nodes: {controlPlane: [{hosts: [a]}]}}", "cluster.nodes.controlPlane[0].name"},
		{"duplicate pool", "cluster: {nodes: {" + cp + ", worker: [{name: a, hosts: [b]}]}}", "pool a is already defined at cluster.nodes.controlPlane[0]"},
		{"no machines", "cluster: {nodes: {controlPlane: [{name: a}]}}", "exactly one of hosts"},
		{"hosts and count", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], count: 1}]}}", "exactly one of hosts"},
		{"missing host", "cluster: {nodes: {controlPlane: [{name: a, hosts: ['']}]}}", "cluster.nodes.controlPlane[0].hosts[0]: hostname or IP is required"},
		{"duplicate host", "cluster: {nodes: {" + cp + ", worker: [{name: w, hosts: [b, a]}]}}", "cluster.nodes.worker[0].hosts[1]: a is already listed at cluster.nodes.controlPlane[0].hosts[0]"},
		{"managed settings on hosts", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], incus: {cpus: 2}}]}}", "only apply to managed pools"},
		{"incus without managed pool", "cluster: {nodes: {" + cp + "}}\nincus: {remote: x}", "incus settings only apply"},
		{"public key without managed pool", "cluster: {ssh: {publicKey: id.pub}, nodes: {" + cp + "}}", "publicKey is only used"},
		{"bad label", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], labels: {'bad key': x}}]}}", "cluster.nodes.controlPlane[0].labels"},
		{"reserved label", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], labels: {quick8s.io/pool: x}}]}}", "reserved"},
		{"bad taint", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], taints: [gpu]}]}}", "cluster.nodes.controlPlane[0].taints[0]"},
		{"duplicate taint", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], taints: [a=1:NoSchedule, a=2:NoSchedule]}]}}", "a:NoSchedule is already listed"},
		{"bare kubernetes version", "cluster: {kubernetesVersion: v1.33.4, nodes: {" + cp + "}}", "full release tag"},
		{"bad port", "cluster: {ssh: {port: 70000}, nodes: {" + cp + "}}", "cluster.ssh.port"},
		{"k3s list", "cluster: {nodes: {" + cp + "}}\nk3s: {server: [a]}", "k3s.server must be a mapping"},
		{"unused k3s list", "cluster: {nodes: {" + cp + "}}\nk3s: {agent: [a]}", "k3s.agent must be a mapping"},
		{"pool k3s list", "cluster: {nodes: {controlPlane: [{name: a, hosts: [a], k3s: [x]}]}}", "cluster.nodes.controlPlane[0].k3s must be a mapping"},
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
