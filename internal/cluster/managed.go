package cluster

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"
)

// Incus holds the top-level incus settings: the connection to a configured
// Incus remote (not credentials, which stay in the Incus client config),
// and machine defaults. A managed pool's own incus settings override them.
type Incus struct {
	Remote      string `yaml:"remote"`
	Project     string `yaml:"project"`
	Image       string `yaml:"image"`
	CPUs        int    `yaml:"cpus"`
	Memory      string `yaml:"memory"`
	Disk        string `yaml:"disk"`
	Network     string `yaml:"network"`
	StoragePool string `yaml:"storagePool"`
}

// over returns i with its unset fields taken from defaults.
func (i Incus) over(defaults Incus) Incus {
	pick := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	if i.CPUs == 0 {
		i.CPUs = defaults.CPUs
	}
	i.Remote, i.Project, i.Image = pick(i.Remote, defaults.Remote), pick(i.Project, defaults.Project), pick(i.Image, defaults.Image)
	i.Memory, i.Disk = pick(i.Memory, defaults.Memory), pick(i.Disk, defaults.Disk)
	i.Network, i.StoragePool = pick(i.Network, defaults.Network), pick(i.StoragePool, defaults.StoragePool)
	return i
}

// StaticNetwork is a managed machine's guest network configuration.
type StaticNetwork struct {
	// Address is an IPv4 CIDR, e.g. 192.168.50.10/24.
	Address       string
	Gateway       string
	Nameservers   []string
	SearchDomains []string
}

// Managed is the single-machine MVP's validated provisioning input.
type Managed struct {
	// Name is the machine's name, and its guest hostname.
	Name  string
	Incus Incus
	// CloudInit is native cloud-config merged into quick8s's own.
	CloudInit map[string]any
	// Network is nil for DHCP.
	Network *StaticNetwork
	// AuthorizedKey is the public key authorized for cluster.ssh.user:
	// cluster.ssh.publicKey, else cluster.ssh.key's .pub. Empty means the
	// SSH agent's key, which needs the agent to resolve, so happens outside
	// this package (lifecycle, via node.AgentKey).
	AuthorizedKey string
}

var guestUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
var capacity = regexp.MustCompile(`^[1-9][0-9]*(MiB|GiB|TiB)$`)

// managed validates the single managed pool at path, against the
// top-level incus defaults, and sets s.Managed.
func (s *Spec) managed(path string, p pool, defaults Incus, publicKey, dir string) error {
	if !resourceName.MatchString(s.Name) {
		return errors.New("cluster.name must be a lowercase name for managed clusters")
	}
	if p.Count > 1 || len(p.Addresses) > 1 {
		return fmt.Errorf("%s: the provider MVP supports a single machine (count: 1, or one address)", path)
	}
	if p.Count < 0 {
		return fmt.Errorf("%s.count must be positive", path)
	}
	i := p.Incus.over(defaults).over(Incus{Project: "default", CPUs: 2, Memory: "4GiB", Disk: "20GiB"})
	if i.Remote == "" || i.Image == "" || i.Network == "" || i.StoragePool == "" {
		return fmt.Errorf("%s: incus.remote, incus.image, incus.network and incus.storagePool are required, at the top level or on the pool", path)
	}
	if i.CPUs < 1 || !capacity.MatchString(i.Memory) || !capacity.MatchString(i.Disk) {
		return fmt.Errorf("%s: incus.cpus must be positive; memory and disk must use MiB, GiB or TiB", path)
	}
	if s.SSH.User == "" || !guestUser.MatchString(s.SSH.User) {
		return errors.New("cluster.ssh.user must name the managed guest account")
	}
	var key string
	if publicKey != "" {
		var err error
		if key, err = authorizedKey(publicKey, dir); err != nil {
			return err
		}
	}
	network, err := staticNetwork(path, p)
	if err != nil {
		return err
	}
	if err := checkCloudInit(p.CloudInit); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.Managed = &Managed{Name: s.Name + "-" + p.Name + "-001", Incus: i, CloudInit: p.CloudInit, Network: network, AuthorizedKey: key}
	if len(s.Managed.Name) > 63 {
		return errors.New("combined cluster and pool name exceeds 63 characters")
	}
	return nil
}

// authorizedKey reads exactly one SSH public key from path.
func authorizedKey(path, dir string) (string, error) {
	path, err := resolvePath(path, dir)
	if err != nil {
		return "", fmt.Errorf("cluster.ssh.publicKey: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading SSH public key: %w", err)
	}
	_, _, _, rest, err := ssh.ParseAuthorizedKey(data)
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return "", fmt.Errorf("%s must contain exactly one SSH public key", path)
	}
	return strings.TrimSpace(string(data)), nil
}

func staticNetwork(path string, p pool) (*StaticNetwork, error) {
	if len(p.Addresses) == 0 {
		if p.Gateway != "" || len(p.Nameservers) != 0 || len(p.SearchDomains) != 0 {
			return nil, fmt.Errorf("%s: gateway, nameservers and searchDomains require static addresses", path)
		}
		return nil, nil
	}
	prefix, err := netip.ParsePrefix(p.Addresses[0])
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsGlobalUnicast() {
		return nil, fmt.Errorf("%s.addresses[0] must be a unicast IPv4 CIDR, e.g. 192.168.50.10/24", path)
	}
	gw, err := netip.ParseAddr(p.Gateway)
	if err != nil || !gw.Is4() || !prefix.Contains(gw) || gw == prefix.Addr() {
		return nil, fmt.Errorf("%s.gateway must be a different IPv4 address in the address's subnet", path)
	}
	for i, dns := range p.Nameservers {
		if _, err := netip.ParseAddr(dns); err != nil {
			return nil, fmt.Errorf("%s.nameservers[%d]: invalid address %q", path, i, dns)
		}
	}
	return &StaticNetwork{Address: p.Addresses[0], Gateway: p.Gateway, Nameservers: p.Nameservers, SearchDomains: p.SearchDomains}, nil
}

// checkCloudInit rejects cloud-config keys quick8s sets itself.
func checkCloudInit(config map[string]any) error {
	for k, v := range config {
		switch k {
		case "users", "user", "ssh_authorized_keys", "ssh_deletekeys", "ssh_keys", "hostname", "fqdn", "preserve_hostname", "network", "merge_how", "merge_type":
			return fmt.Errorf("cloudInit.%s conflicts with quick8s-managed guest identity or networking", k)
		case "packages":
			if _, ok := v.([]any); !ok {
				return errors.New("cloudInit.packages must be a list")
			}
		}
	}
	return nil
}

// GuestData combines native cloud-config with quick8s's required SSH account,
// and renders the static network configuration, if any, matched by MAC.
func (s *Spec) GuestData(macAddress string) (string, string, error) {
	m := s.Managed
	if err := checkCloudInit(m.CloudInit); err != nil {
		return "", "", err
	}
	data := make(map[string]any)
	for k, v := range m.CloudInit {
		data[k] = v
	}
	// Ensure the guest has the tools quick8s needs, preserving user packages.
	packages, _ := data["packages"].([]any)
	data["packages"] = append(packages, "openssh-server", "sudo", "curl", "ca-certificates")
	data["hostname"] = m.Name
	data["users"] = []any{map[string]any{
		"name": s.SSH.User, "shell": "/bin/sh", "lock_passwd": true,
		"sudo": "ALL=(ALL) NOPASSWD:ALL", "ssh_authorized_keys": []string{m.AuthorizedKey},
	}}
	user, err := yaml.Marshal(data)
	if err != nil {
		return "", "", err
	}
	var network string
	if n := m.Network; n != nil {
		netData, err := yaml.Marshal(map[string]any{"version": 2, "ethernets": map[string]any{"eth0": map[string]any{
			"match": map[string]any{"macaddress": macAddress}, "set-name": "eth0",
			"dhcp4": false, "dhcp6": false, "addresses": []string{n.Address},
			"routes":      []any{map[string]any{"to": "0.0.0.0/0", "via": n.Gateway}},
			"nameservers": map[string]any{"addresses": n.Nameservers, "search": n.SearchDomains},
		}}})
		if err != nil {
			return "", "", err
		}
		network = string(netData)
	}
	return "#cloud-config\n" + string(user), network, nil
}
