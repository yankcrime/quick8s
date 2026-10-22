// Package incus provisions virtual machines through the upstream Incus API client.
package incus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	client "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"

	"quick8s/internal/cluster"
	"quick8s/internal/provider"
)

const ownerKey = "user.quick8s.cluster-id"
const hashKey = "user.quick8s.config-hash"

type Provider struct {
	server   client.InstanceServer
	config   *cliconfig.Config
	opts     cluster.Incus
	Endpoint string
	Project  string
}

// Connect reuses Incus CLI credentials without invoking the CLI or enrolling clients.
func Connect(opts cluster.Incus) (*Provider, error) {
	conf, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, err
	}
	remote, ok := conf.Remotes[opts.Remote]
	if !ok {
		return nil, fmt.Errorf("Incus remote %q is not configured; enroll it with incus remote add", opts.Remote)
	}
	if remote.AuthType != "" && remote.AuthType != "tls" {
		return nil, errors.New("the provider MVP supports TLS client certificates and Unix sockets; OIDC is not yet supported")
	}
	server, err := conf.GetInstanceServer(opts.Remote)
	if err != nil {
		return nil, fmt.Errorf("connecting to Incus remote %s: %w", opts.Remote, err)
	}
	server = server.UseProject(opts.Project)
	if httpClient, err := server.GetHTTPClient(); err == nil {
		httpClient.Timeout = 30 * time.Second
	}
	info, err := server.GetConnectionInfo()
	if err != nil {
		server.Disconnect()
		return nil, err
	}
	endpoint := info.URL
	if endpoint == "" {
		endpoint = "unix:" + info.SocketPath
	}
	return &Provider{server: server, config: conf, opts: opts, Endpoint: endpoint, Project: opts.Project}, nil
}

func (p *Provider) Close() { p.server.Disconnect() }

// Validate checks existing infrastructure without creating or changing it.
func (p *Provider) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.server.HasExtension("virtual-machines") {
		return errors.New("Incus server does not support virtual machines")
	}
	if _, _, err := p.server.GetProject(p.Project); err != nil {
		return fmt.Errorf("Incus project %s: %w", p.Project, err)
	}
	network, _, err := p.server.GetNetwork(p.opts.Network)
	if err != nil {
		return fmt.Errorf("Incus network %s: %w", p.opts.Network, err)
	}
	if !network.Managed {
		return fmt.Errorf("network %s must be managed by Incus", p.opts.Network)
	}
	if _, _, err := p.server.GetStoragePool(p.opts.StoragePool); err != nil {
		return fmt.Errorf("Incus storage pool %s: %w", p.opts.StoragePool, err)
	}
	return nil
}

func (p *Provider) Inspect(ctx context.Context, name string) (*provider.Machine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instance, _, err := p.server.GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if instance.Type != string(api.InstanceTypeVM) {
		return nil, fmt.Errorf("%s exists but is not a VM", name)
	}
	state, _, err := p.server.GetInstanceState(name)
	if err != nil {
		return nil, err
	}
	m := &provider.Machine{ID: instance.Config["volatile.uuid"], Name: name, Owner: instance.Config[ownerKey], ConfigHash: instance.Config[hashKey], State: state.Status, Image: instance.Config["volatile.base_image"]}
	mac := instance.ExpandedDevices["eth0"]["hwaddr"]
	if mac == "" {
		mac = instance.Config["volatile.eth0.hwaddr"]
	}
	for name, nic := range state.Network {
		if (mac != "" && !strings.EqualFold(nic.Hwaddr, mac)) || (mac == "" && name != "eth0") {
			continue
		}
		for _, addr := range nic.Addresses {
			ip, err := netip.ParseAddr(addr.Address)
			if err == nil && ip.Is4() && ip.IsGlobalUnicast() {
				m.Addresses = append(m.Addresses, ip.String())
			}
		}
	}
	slices.Sort(m.Addresses)
	return m, nil
}

func (p *Provider) imageSource() (api.InstanceSource, error) {
	remote, name, err := p.config.ParseRemote(p.opts.Image)
	if err != nil {
		return api.InstanceSource{}, err
	}
	if remote == p.opts.Remote {
		alias, _, err := p.server.GetImageAliasType("virtual-machine", name)
		if err == nil {
			name = alias.Target
		} else if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return api.InstanceSource{}, err
		}
		image, _, err := p.server.GetImage(name)
		if err != nil {
			return api.InstanceSource{}, err
		}
		if image.Type != "virtual-machine" {
			return api.InstanceSource{}, errors.New("image must be a virtual-machine image")
		}
		return api.InstanceSource{Type: "image", Fingerprint: image.Fingerprint}, nil
	}
	r, ok := p.config.Remotes[remote]
	if !ok || !r.Public {
		return api.InstanceSource{}, errors.New("MVP image source must be local to the target remote or on a configured public image remote")
	}
	source, err := p.config.GetImageServer(remote)
	if err != nil {
		return api.InstanceSource{}, err
	}
	defer source.Disconnect()
	if c, err := source.GetHTTPClient(); err == nil {
		c.Timeout = 30 * time.Second
	}
	alias, _, err := source.GetImageAliasType("virtual-machine", name)
	if err == nil {
		name = alias.Target
	} else if !api.StatusErrorCheck(err, http.StatusNotFound) {
		return api.InstanceSource{}, err
	}
	image, _, err := source.GetImage(name)
	if err != nil {
		return api.InstanceSource{}, err
	}
	if image.Type != "virtual-machine" {
		return api.InstanceSource{}, errors.New("image must be a virtual-machine image")
	}
	info, err := source.GetConnectionInfo()
	if err != nil {
		return api.InstanceSource{}, err
	}
	return api.InstanceSource{Type: "image", Mode: "pull", Server: info.URL, Protocol: info.Protocol, Certificate: info.Certificate, Fingerprint: image.Fingerprint}, nil
}

func (p *Provider) Create(ctx context.Context, req provider.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := p.imageSource()
	if err != nil {
		return fmt.Errorf("resolving VM image %s: %w", p.opts.Image, err)
	}
	op, err := p.server.CreateInstance(p.request(req, source))
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

// Check compares live configuration as well as ownership metadata. An operator's
// manual edits must not be hidden by the original config-hash tag.
func (p *Provider) Check(ctx context.Context, req provider.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i, _, err := p.server.GetInstance(req.Name)
	if err != nil {
		return err
	}
	expected := p.request(req, api.InstanceSource{})
	for key, value := range expected.Config {
		if i.ExpandedConfig[key] != value {
			return fmt.Errorf("VM configuration drift at %s; MVP requires explicit recreation", key)
		}
	}
	if i.ExpandedConfig["cloud-init.network-config"] != req.NetworkData || len(i.Profiles) != 0 || !reflect.DeepEqual(i.ExpandedDevices, expected.Devices) {
		return errors.New("VM devices, profiles or network configuration changed; MVP requires explicit recreation")
	}
	return nil
}

func (p *Provider) request(req provider.Request, source api.InstanceSource) api.InstancesPost {
	config := api.ConfigMap{
		ownerKey: req.Owner, hashKey: req.ConfigHash,
		"limits.cpu": strconv.Itoa(p.opts.CPUs), "limits.memory": p.opts.Memory,
		"cloud-init.user-data": req.UserData,
	}
	if req.NetworkData != "" {
		config["cloud-init.network-config"] = req.NetworkData
	}
	return api.InstancesPost{
		Name: req.Name, Type: api.InstanceTypeVM, Source: source,
		InstancePut: api.InstancePut{
			Config: config, Profiles: []string{},
			Devices: api.DevicesMap{
				"root": {"type": "disk", "path": "/", "pool": p.opts.StoragePool, "size": p.opts.Disk},
				"eth0": {"type": "nic", "network": p.opts.Network, "name": "eth0", "hwaddr": req.MACAddress},
			},
		},
	}
}

func (p *Provider) Start(ctx context.Context, name string) error {
	op, err := p.server.UpdateInstanceState(name, api.InstanceStatePut{Action: "start"}, "")
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

func (p *Provider) Delete(ctx context.Context, name string) error {
	m, err := p.Inspect(ctx, name)
	if err != nil || m == nil {
		return err
	}
	if strings.EqualFold(m.State, "running") {
		op, err := p.server.UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Timeout: 30, Force: true}, "")
		if err != nil {
			return err
		}
		if err := op.WaitContext(ctx); err != nil {
			return err
		}
	}
	op, err := p.server.DeleteInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}
