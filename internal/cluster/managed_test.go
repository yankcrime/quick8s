package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"
)

const managedYAML = `
cluster:
  name: test
  ssh: {user: quick8s, publicKey: ~/id.pub}
  nodes:
    controlPlane:
      - name: servers
        count: 1
        incus: {memory: 8GiB}
        labels: {tier: control}
        cloudInit:
          packages: [jq]
          write_files:
            - path: /etc/example
              content: hello
incus: {remote: quick8s, image: 'images:debian/12/cloud', network: incusbr0, storagePool: local, cpus: 4, memory: 4GiB}
`

// keyDir writes the fixture's public key, in a temporary home directory so
// the ~ in its path is expanded too.
func keyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ssh.NewPublicKey(pub)
	if err := os.WriteFile(filepath.Join(dir, "id.pub"), ssh.MarshalAuthorizedKey(key), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestManagedGuestData(t *testing.T) {
	s, err := parse([]byte(managedYAML), keyDir(t))
	if err != nil {
		t.Fatal(err)
	}
	// Pool settings override the top-level defaults, which override quick8s's.
	want := Incus{Remote: "quick8s", Project: "default", Image: "images:debian/12/cloud", CPUs: 4, Memory: "8GiB", Disk: "20GiB", Network: "incusbr0", StoragePool: "local"}
	if s.Managed.Name != "test-servers-001" || s.Managed.Incus != want || !strings.HasPrefix(s.Managed.AuthorizedKey, "ssh-ed25519 ") {
		t.Fatalf("managed = %+v", s.Managed)
	}
	if len(s.ControlPlanes) != 1 || s.ControlPlanes[0].Host != "" || s.ControlPlanes[0].Metadata.Labels["tier"] != "control" {
		t.Fatalf("control planes = %+v", s.ControlPlanes)
	}
	user, network, err := s.GuestData("02:00:00:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if network != "" {
		t.Fatalf("DHCP unexpectedly overridden: %s", network)
	}
	var cloud map[string]any
	if err := yaml.Unmarshal([]byte(user), &cloud); err != nil {
		t.Fatal(err)
	}
	users := cloud["users"].([]any)
	if users[0].(map[string]any)["name"] != "quick8s" || cloud["hostname"] != "test-servers-001" {
		t.Fatal(cloud)
	}
	if !strings.Contains(user, "jq") || !strings.Contains(user, "openssh-server") || !strings.Contains(user, "/etc/example") {
		t.Fatal(user)
	}
	s.Managed.Network = &StaticNetwork{Address: "192.168.50.10/24", Gateway: "192.168.50.1", Nameservers: []string{"192.168.50.1"}}
	_, network, err = s.GuestData("02:00:00:00:00:01")
	if err != nil || !strings.Contains(network, "dhcp4: false") || !strings.Contains(network, "192.168.50.10/24") || !strings.Contains(network, "02:00:00:00:00:01") {
		t.Fatalf("network=%s err=%v", network, err)
	}
}

func TestManagedStaticAddress(t *testing.T) {
	input := strings.Replace(managedYAML, "count: 1", "addresses: [192.168.50.10/24]\n        gateway: 192.168.50.1\n        nameservers: [192.168.50.1]", 1)
	s, err := parse([]byte(input), keyDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Managed.Network; n == nil || n.Address != "192.168.50.10/24" || n.Gateway != "192.168.50.1" {
		t.Fatalf("network = %+v", s.Managed.Network)
	}
}

func TestManagedValidation(t *testing.T) {
	for _, tt := range []struct{ name, old, replacement, want string }{
		{"multi node", "count: 1", "count: 2", "single machine"},
		{"multiple addresses", "count: 1", "addresses: [192.168.1.2/24, 192.168.1.3/24]\n        gateway: 192.168.1.1", "single machine"},
		{"other pools", "    controlPlane:", "    worker: [{name: w, hosts: [w]}]\n    controlPlane:", "no other pools"},
		{"missing name", "name: test", "name: ''", "cluster.name"},
		{"unknown field", "memory: 8GiB", "memroy: 8GiB", "field memroy"},
		{"negative cpu", "memory: 8GiB", "cpus: -1", "positive"},
		{"bad size", "memory: 8GiB", "memory: '4 bananas'", "MiB"},
		{"missing remote", "remote: quick8s, ", "", "incus.remote"},
		{"unreadable public key", "publicKey: ~/id.pub", "publicKey: missing.pub", "reading SSH public key"},
		{"bad address", "count: 1", "addresses: [192.168.1.2]", "cluster.nodes.controlPlane[0].addresses[0]"},
		{"missing gateway", "count: 1", "addresses: [192.168.1.2/24]", "cluster.nodes.controlPlane[0].gateway"},
		{"dhcp nameservers", "count: 1", "count: 1\n        nameservers: [1.1.1.1]", "require static addresses"},
		{"reserved cloud-init key", "packages: [jq]", "users: []", "cloudInit.users conflicts"},
		{"packages not a list", "packages: [jq]", "packages: jq", "cloudInit.packages must be a list"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(managedYAML, tt.old, tt.replacement, 1)
			if input == managedYAML {
				t.Fatalf("%q not found in the fixture", tt.old)
			}
			_, err := parse([]byte(input), keyDir(t))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
}

// Without publicKey, a configured key file's .pub is authorized, so the VM
// doesn't depend on what the agent holds; without either, the key comes
// from the SSH agent, which parsing doesn't touch.
func TestManagedPublicKeyDefaults(t *testing.T) {
	dir := keyDir(t)
	s, err := parse([]byte(strings.Replace(managedYAML, "publicKey: ~/id.pub", "key: ~/id", 1)), dir)
	if err != nil || !strings.HasPrefix(s.Managed.AuthorizedKey, "ssh-ed25519 ") {
		t.Fatalf("authorized key = %q, err = %v", s.Managed.AuthorizedKey, err)
	}
	s, err = parse([]byte(strings.Replace(managedYAML, ", publicKey: ~/id.pub", "", 1)), dir)
	if err != nil || s.Managed.AuthorizedKey != "" {
		t.Fatalf("authorized key = %q, err = %v", s.Managed.AuthorizedKey, err)
	}
}

func TestLoadIncusExample(t *testing.T) {
	s, err := Load("../../examples/incus.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := s.Managed
	// The pool's memory overrides the top-level default; its key comes
	// from the SSH agent, since the example leaves publicKey unset.
	if m == nil || m.Name != "development-servers-001" || m.Incus.Memory != "8GiB" || m.Incus.CPUs != 2 || m.AuthorizedKey != "" {
		t.Fatalf("managed = %+v", m)
	}
	if got := s.ControlPlanes[0].Metadata.Labels["tier"]; got != "control" {
		t.Fatalf("tier label = %q", got)
	}
}
