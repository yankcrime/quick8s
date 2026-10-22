package incus

import (
	"context"
	"testing"

	client "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"quick8s/internal/cluster"
	"quick8s/internal/provider"
)

type observationServer struct {
	client.InstanceServer
	instance api.Instance
	state    api.InstanceState
}

func (s *observationServer) GetInstance(string) (*api.Instance, string, error) {
	return &s.instance, "", nil
}
func (s *observationServer) GetInstanceState(string) (*api.InstanceState, string, error) {
	return &s.state, "", nil
}

func TestInspectMatchesGuestNICByMAC(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		s := &observationServer{instance: api.Instance{Type: "virtual-machine"}, state: api.InstanceState{Status: "Running"}}
		s.instance.Name = "test"
		s.instance.Config = api.ConfigMap{ownerKey: "owner", hashKey: "hash"}
		mac := "02:00:00:00:00:01"
		if explicit {
			s.instance.ExpandedDevices = api.DevicesMap{"eth0": {"hwaddr": mac}}
		} else {
			s.instance.Config["volatile.eth0.hwaddr"] = mac
		}
		s.state.Network = map[string]api.InstanceStateNetwork{
			"enp5s0": {Hwaddr: mac, Addresses: []api.InstanceStateNetworkAddress{{Address: "192.168.1.117"}, {Address: "fe80::1"}}},
			"other":  {Hwaddr: "02:00:00:00:00:02", Addresses: []api.InstanceStateNetworkAddress{{Address: "10.0.0.10"}}},
		}
		p := &Provider{server: s}
		m, err := p.Inspect(context.Background(), "test")
		if err != nil || len(m.Addresses) != 1 || m.Addresses[0] != "192.168.1.117" {
			t.Fatalf("explicit=%v observed=%+v err=%v", explicit, m, err)
		}
	}
}

func TestCheckDetectsLiveConfigurationDrift(t *testing.T) {
	req := provider.Request{Name: "test", Owner: "owner", ConfigHash: "hash", UserData: "#cloud-config\n", MACAddress: "02:00:00:00:00:01"}
	p := &Provider{opts: cluster.Incus{CPUs: 2, Memory: "4GiB", Disk: "20GiB", Network: "incusbr0", StoragePool: "local"}}
	expected := p.request(req, api.InstanceSource{})
	s := &observationServer{instance: api.Instance{ExpandedConfig: expected.Config, ExpandedDevices: expected.Devices}}
	p.server = s
	if err := p.Check(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s.instance.ExpandedConfig["limits.cpu"] = "4"
	if err := p.Check(context.Background(), req); err == nil {
		t.Fatal("missed manual CPU drift despite matching hash tag")
	}
}

func TestCreateRequestIsIsolatedVM(t *testing.T) {
	p := &Provider{opts: cluster.Incus{CPUs: 2, Memory: "4GiB", Disk: "20GiB", Network: "incusbr0", StoragePool: "local"}}
	req := p.request(provider.Request{Name: "test-server", Owner: "owner", ConfigHash: "hash", UserData: "#cloud-config\n", NetworkData: "version: 2\n"}, api.InstanceSource{Type: "image", Fingerprint: "pinned"})
	if req.Type != api.InstanceTypeVM || req.Start || req.Ephemeral {
		t.Fatalf("unexpected VM lifecycle: %+v", req)
	}
	if req.Profiles == nil || len(req.Profiles) != 0 {
		t.Fatal("must not inherit default profile's user-data/devices")
	}
	if req.Config[ownerKey] != "owner" || req.Config[hashKey] != "hash" || req.Source.Fingerprint != "pinned" {
		t.Fatal(req)
	}
	if req.Devices["root"]["pool"] != "local" || req.Devices["eth0"]["network"] != "incusbr0" || req.Config["cloud-init.network-config"] == "" {
		t.Fatal(req)
	}
}
