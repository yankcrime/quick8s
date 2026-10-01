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
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultFile is the cluster definition `up` and `down` read from the
// current directory, much like Terraform reads *.tf files.
const DefaultFile = "quick8s.yaml"

// Spec is a validated cluster definition.
type Spec struct {
	// KubernetesVersion is the K3s release to install, e.g.
	// "v1.33.4+k3s1". Empty means K3s's latest stable release.
	KubernetesVersion string
	SSH               SSH
	// ControlPlanes lists K3s server nodes. The first one initializes a new
	// cluster; the rest join it with embedded etcd.
	ControlPlanes []Node
	Workers       []Node
	// ServerConfig and AgentConfig are K3s config.yaml contents for control
	// plane and worker nodes respectively, or nil when not given.
	ServerConfig []byte
	AgentConfig  []byte
}

// SSH holds connection settings shared by every node. Zero values mean
// the CLI's own defaults.
type SSH struct {
	User string
	Port int
	// Key is an absolute path to a private key offered after the SSH agent.
	Key string
}

// Node is a machine in the cluster, addressed by hostname or IP.
type Node struct {
	Host string
}

// file is the on-disk shape of quick8s.yaml: the cluster's shape under a
// top-level cluster key, with nodes listed by address, and K3s's own
// configuration in a separate top-level k3s key.
type file struct {
	Cluster struct {
		KubernetesVersion string `yaml:"kubernetesVersion"`
		SSH               struct {
			User string `yaml:"user"`
			Port int    `yaml:"port"`
			Key  string `yaml:"key"`
		} `yaml:"ssh"`
		Nodes struct {
			ControlPlane []string `yaml:"controlPlane"`
			Worker       []string `yaml:"worker"`
		} `yaml:"nodes"`
	} `yaml:"cluster"`
	// K3s sections are K3s's own config.yaml format, passed through as-is.
	K3s struct {
		Server yaml.Node `yaml:"server"`
		Agent  yaml.Node `yaml:"agent"`
	} `yaml:"k3s"`
}

// k3sVersion matches K3s release tags; the "+k3sN" suffix is required to
// download a release.
var k3sVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-rc\d+)?\+k3s\d+$`)

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
	if c.KubernetesVersion != "" && !k3sVersion.MatchString(c.KubernetesVersion) {
		return nil, fmt.Errorf("cluster.kubernetesVersion %q isn't a K3s release; use the full release tag, e.g. v1.33.4+k3s1 (see https://github.com/k3s-io/k3s/releases)", c.KubernetesVersion)
	}
	if c.SSH.Port < 0 || c.SSH.Port > 65535 {
		return nil, fmt.Errorf("cluster.ssh.port %d is out of range", c.SSH.Port)
	}
	if len(c.Nodes.ControlPlane) == 0 {
		return nil, errors.New("at least one entry under cluster.nodes.controlPlane is required")
	}

	spec := &Spec{
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

	seen := map[string]string{}
	nodes := func(section string, hosts []string) ([]Node, error) {
		var out []Node
		for i, host := range hosts {
			host = strings.TrimSpace(host)
			if host == "" {
				return nil, fmt.Errorf("%s[%d]: hostname or IP is required", section, i)
			}
			if prev, ok := seen[host]; ok {
				return nil, fmt.Errorf("%s[%d]: %s is already listed under %s", section, i, host, prev)
			}
			seen[host] = section
			out = append(out, Node{Host: host})
		}
		return out, nil
	}
	var err error
	if spec.ControlPlanes, err = nodes("cluster.nodes.controlPlane", c.Nodes.ControlPlane); err != nil {
		return nil, err
	}
	if spec.Workers, err = nodes("cluster.nodes.worker", c.Nodes.Worker); err != nil {
		return nil, err
	}

	if spec.ServerConfig, err = k3sConfig("k3s.server", &f.K3s.Server); err != nil {
		return nil, err
	}
	if spec.AgentConfig, err = k3sConfig("k3s.agent", &f.K3s.Agent); err != nil {
		return nil, err
	}
	return spec, nil
}

// k3sConfig re-encodes a k3s section as a standalone config.yaml. quick8s
// doesn't interpret the keys; K3s validates them itself when it starts.
func k3sConfig(section string, n *yaml.Node) ([]byte, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.MappingNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return yaml.Marshal(n)
	default:
		return nil, fmt.Errorf("%s must be a mapping of K3s config keys, e.g. `disable: [traefik]`", section)
	}
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
