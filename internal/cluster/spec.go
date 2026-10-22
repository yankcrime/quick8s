// Package cluster reads a declarative cluster definition (quick8s.yaml) and
// works out what `quick8s up` has to do to make the nodes match it.
package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"quick8s/internal/k3s"
)

// DefaultFile is the cluster definition `up` and `down` read from the
// current directory, much like Terraform reads *.tf files.
const DefaultFile = "quick8s.yaml"

// Spec is a validated cluster definition.
type Spec struct {
	Name string
	// Managed is set for provider-backed clusters; addresses are resolved before K3s planning.
	Managed *Managed
	// KubernetesVersion is the K3s release to install, or upgrade existing
	// nodes to, e.g. "v1.33.4+k3s1". Empty means K3s's latest stable
	// release when creating the cluster, and never upgrading it.
	KubernetesVersion string
	SSH               SSH
	// ControlPlanes lists K3s server nodes. The first one initializes a new
	// cluster; the rest join it with embedded etcd.
	ControlPlanes []Node
	Workers       []Node
}

// SSH holds connection settings shared by every node. Zero values mean
// the CLI's own defaults.
type SSH struct {
	User string
	Port int
	// Key is an absolute path to a private key offered after the SSH
	// agent's keys, or empty to use the agent alone.
	Key string
}

// Node is a machine in the cluster, addressed by hostname or IP.
type Node struct {
	// Host is empty for a managed node until its machine is provisioned.
	Host string
	Pool string
	// Config is the node's K3s config.yaml: its role's k3s section with its
	// pool's k3s section merged over it, or nil when neither is given, in
	// which case the node shouldn't have a config file.
	Config []byte
	// Metadata is set through the Kubernetes API, not K3s's config. It's
	// nil unless the node's pool declares labels or taints, in which case
	// up leaves the node's labels and taints alone and makes no API calls
	// for it. An empty labels: {} or taints: [] still opts in, so removing
	// a pool's last label clears what quick8s set.
	Metadata *k3s.NodeMetadata
}

// file is the on-disk shape of quick8s.yaml, one top-level key per system:
// cluster for quick8s's own settings and node pools, incus for where
// managed machines come from, and k3s for K3s's own configuration.
type file struct {
	Cluster struct {
		Name              string `yaml:"name"`
		KubernetesVersion string `yaml:"kubernetesVersion"`
		SSH               struct {
			User      string `yaml:"user"`
			Port      int    `yaml:"port"`
			Key       string `yaml:"key"`
			PublicKey string `yaml:"publicKey"`
		} `yaml:"ssh"`
		Nodes struct {
			ControlPlane []pool `yaml:"controlPlane"`
			Worker       []pool `yaml:"worker"`
		} `yaml:"nodes"`
	} `yaml:"cluster"`
	Incus Incus `yaml:"incus"`
	// K3s sections are K3s's own config.yaml format, passed through as-is.
	K3s struct {
		Server yaml.Node `yaml:"server"`
		Agent  yaml.Node `yaml:"agent"`
	} `yaml:"k3s"`
}

// pool is a set of nodes sharing a role and settings: either existing
// machines listed by address (hosts), or machines quick8s provisions
// (count for DHCP, or one static address per machine).
type pool struct {
	Name      string   `yaml:"name"`
	Hosts     []string `yaml:"hosts"`
	Count     int      `yaml:"count"`
	Addresses []string `yaml:"addresses"`
	// Guest network settings for static addresses.
	Gateway       string   `yaml:"gateway"`
	Nameservers   []string `yaml:"nameservers"`
	SearchDomains []string `yaml:"searchDomains"`
	// Overrides of the top-level incus settings.
	Incus Incus `yaml:"incus"`
	// Native cloud-config, passed through except for keys quick8s owns.
	CloudInit map[string]any `yaml:"cloudInit"`
	// Merged over the role's k3s section for this pool's nodes.
	K3s    yaml.Node         `yaml:"k3s"`
	Labels map[string]string `yaml:"labels"`
	Taints []string          `yaml:"taints"`
}

func (p pool) managed() bool { return p.Count != 0 || len(p.Addresses) != 0 }

var resourceName = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Load reads and validates the cluster definition at path.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && path == DefaultFile {
		return nil, fmt.Errorf("no %s in the current directory; create one, or point at a cluster definition with --file", DefaultFile)
	}
	if err != nil {
		return nil, fmt.Errorf("reading cluster definition: %w", err)
	}

	spec, err := parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// parse decodes and validates a cluster definition. Relative SSH key paths
// are resolved against dir, the directory the definition lives in.
func parse(data []byte, dir string) (*Spec, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("cluster definition is empty")
		}
		return nil, err
	}

	c := f.Cluster
	if c.KubernetesVersion != "" {
		if _, err := k3s.ParseRelease(c.KubernetesVersion); err != nil {
			return nil, fmt.Errorf("cluster.kubernetesVersion: %w", err)
		}
	}
	if c.SSH.Port < 0 || c.SSH.Port > 65535 {
		return nil, fmt.Errorf("cluster.ssh.port %d is out of range", c.SSH.Port)
	}
	if len(c.Nodes.ControlPlane) == 0 {
		return nil, errors.New("at least one pool under cluster.nodes.controlPlane is required")
	}

	spec := &Spec{
		Name:              c.Name,
		KubernetesVersion: c.KubernetesVersion,
		SSH:               SSH{User: c.SSH.User, Port: c.SSH.Port},
	}
	if c.SSH.Key != "" {
		key, err := resolvePath(c.SSH.Key, dir)
		if err != nil {
			return nil, fmt.Errorf("cluster.ssh.key: %w", err)
		}
		spec.SSH.Key = key
	}
	for path, section := range map[string]*yaml.Node{"k3s.server": &f.K3s.Server, "k3s.agent": &f.K3s.Agent} {
		if err := checkMapping(path, section); err != nil {
			return nil, err
		}
	}

	type managedPool struct {
		path string
		pool pool
		node Node
	}
	var managed []managedPool
	pools := map[string]string{}
	hosts := map[string]string{}
	for _, role := range []struct {
		name    string
		pools   []pool
		section *yaml.Node
		nodes   *[]Node
	}{
		{"controlPlane", c.Nodes.ControlPlane, &f.K3s.Server, &spec.ControlPlanes},
		{"worker", c.Nodes.Worker, &f.K3s.Agent, &spec.Workers},
	} {
		for i, p := range role.pools {
			path := fmt.Sprintf("cluster.nodes.%s[%d]", role.name, i)
			if !resourceName.MatchString(p.Name) {
				return nil, fmt.Errorf("%s.name must be a lowercase name, e.g. general", path)
			}
			if prev, ok := pools[p.Name]; ok {
				return nil, fmt.Errorf("%s: pool %s is already defined at %s", path, p.Name, prev)
			}
			pools[p.Name] = path
			kinds := 0
			for _, set := range []bool{len(p.Hosts) != 0, p.Count != 0, len(p.Addresses) != 0} {
				if set {
					kinds++
				}
			}
			if kinds != 1 {
				return nil, fmt.Errorf("%s: set exactly one of hosts (existing machines), count or addresses (managed machines)", path)
			}
			config, err := mergeConfig(path+".k3s", role.section, &p.K3s)
			if err != nil {
				return nil, err
			}
			metadata, err := nodeMetadata(path, p)
			if err != nil {
				return nil, err
			}
			template := Node{Pool: p.Name, Config: config, Metadata: metadata}
			if p.managed() {
				managed = append(managed, managedPool{path, p, template})
				*role.nodes = append(*role.nodes, template)
				continue
			}
			if !reflect.ValueOf(p.Incus).IsZero() || p.CloudInit != nil || p.Gateway != "" || p.Nameservers != nil || p.SearchDomains != nil {
				return nil, fmt.Errorf("%s: incus, cloudInit and guest network settings only apply to managed pools, not hosts", path)
			}
			for j, host := range p.Hosts {
				host = strings.TrimSpace(host)
				at := fmt.Sprintf("%s.hosts[%d]", path, j)
				if host == "" {
					return nil, fmt.Errorf("%s: hostname or IP is required", at)
				}
				if prev, ok := hosts[host]; ok {
					return nil, fmt.Errorf("%s: %s is already listed at %s", at, host, prev)
				}
				hosts[host] = at
				n := template
				n.Host = host
				*role.nodes = append(*role.nodes, n)
			}
		}
	}

	switch {
	case len(managed) == 0 && !reflect.ValueOf(f.Incus).IsZero():
		return nil, errors.New("incus settings only apply to managed pools, which set count or addresses")
	case len(managed) > 1 || len(managed) == 1 && len(spec.Nodes()) > 1:
		return nil, errors.New("the provider MVP supports a single managed pool, with no other pools")
	case len(managed) == 1:
		m := managed[0]
		// Authorize the configured key file's public half, so the VM
		// doesn't depend on what the agent holds; else the agent's key.
		publicKey := c.SSH.PublicKey
		if publicKey == "" && c.SSH.Key != "" {
			publicKey = c.SSH.Key + ".pub"
		}
		if err := spec.managed(m.path, m.pool, f.Incus, publicKey, dir); err != nil {
			return nil, err
		}
	case c.SSH.PublicKey != "":
		return nil, errors.New("cluster.ssh.publicKey is only used to authorize logins on managed machines")
	}
	return spec, nil
}

// nodeMetadata validates a pool's labels and taints, returning nil when
// the pool declares neither.
func nodeMetadata(path string, p pool) (*k3s.NodeMetadata, error) {
	if p.Labels == nil && p.Taints == nil {
		return nil, nil
	}
	m := &k3s.NodeMetadata{Labels: map[string]string{}}
	for k, v := range p.Labels {
		if err := k3s.ValidateLabel(k, v); err != nil {
			return nil, fmt.Errorf("%s.labels: %w", path, err)
		}
		m.Labels[k] = v
	}
	seen := map[string]bool{}
	for i, s := range p.Taints {
		t, err := k3s.ParseTaint(s)
		if err != nil {
			return nil, fmt.Errorf("%s.taints[%d]: %w", path, i, err)
		}
		id := t.Key + ":" + t.Effect
		if seen[id] {
			return nil, fmt.Errorf("%s.taints[%d]: %s is already listed", path, i, id)
		}
		seen[id] = true
		m.Taints = append(m.Taints, t)
	}
	return m, nil
}

// mergeConfig re-encodes a role's k3s section with a pool's keys merged
// over it, as a standalone config.yaml. quick8s doesn't interpret the keys;
// K3s validates them itself when it starts. The role's key order is kept,
// with the pool's new keys after it.
func mergeConfig(path string, role, pool *yaml.Node) ([]byte, error) {
	if err := checkMapping(path, pool); err != nil {
		return nil, err
	}
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: role.Style}
	merged.Content = append(merged.Content, role.Content...)
	for i := 0; i+1 < len(pool.Content); i += 2 {
		key, value := pool.Content[i], pool.Content[i+1]
		replaced := false
		for j := 0; j+1 < len(merged.Content); j += 2 {
			if merged.Content[j].Value == key.Value {
				merged.Content[j+1], replaced = value, true
			}
		}
		if !replaced {
			merged.Content = append(merged.Content, key, value)
		}
	}
	if len(merged.Content) == 0 {
		return nil, nil
	}
	return yaml.Marshal(merged)
}

func checkMapping(path string, n *yaml.Node) error {
	if n.Kind != 0 && n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping of K3s config keys, e.g. `disable: [traefik]`", path)
	}
	return nil
}

// resolvePath expands a leading ~/ and makes relative paths relative to dir.
func resolvePath(p, dir string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Abs(p)
}
