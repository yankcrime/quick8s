package lifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"quick8s/internal/cluster"
	"quick8s/internal/node/nodetest"
	"quick8s/internal/provider"
	"quick8s/internal/state"
)

type fakeProvider struct {
	machine                  *provider.Machine
	creates, starts, deletes int
	createErr                error
}

func (p *fakeProvider) Validate(context.Context) error                { return nil }
func (p *fakeProvider) Check(context.Context, provider.Request) error { return nil }

func (p *fakeProvider) Inspect(context.Context, string) (*provider.Machine, error) {
	return p.machine, nil
}
func (p *fakeProvider) Create(_ context.Context, r provider.Request) error {
	p.creates++
	p.machine = &provider.Machine{ID: "instance-uuid", Name: r.Name, Owner: r.Owner, ConfigHash: r.ConfigHash, State: "Stopped", Addresses: []string{"192.168.50.10"}, Image: "fingerprint"}
	return p.createErr
}
func (p *fakeProvider) Start(context.Context, string) error {
	p.starts++
	p.machine.State = "Running"
	return nil
}
func (p *fakeProvider) Delete(context.Context, string) error {
	p.deletes++
	p.machine = nil
	return nil
}

func testSession(t *testing.T) (*Session, *fakeProvider) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{}
	s := &Session{provider: p, store: store, endpoint: "https://incus", progress: func(string) {},
		spec: &cluster.Spec{Name: "test", SSH: cluster.SSH{User: "quick8s"},
			Managed: &cluster.Managed{Name: "test-servers-001", AuthorizedKey: "ssh-ed25519 AAAA test",
				Incus: cluster.Incus{Remote: "lab", Project: "default", Image: "images:debian/12/cloud", CPUs: 2}}}}
	t.Cleanup(s.Close)
	return s, p
}

func TestInterruptedCreateIsRediscovered(t *testing.T) {
	s, p := testSession(t)
	p.createErr = errors.New("interrupted operation wait")
	if _, err := s.Ensure(context.Background()); err == nil {
		t.Fatal("expected interruption")
	}
	inv, err := s.store.Load()
	if err != nil || inv == nil || inv.ClusterID == "" {
		t.Fatalf("identity not persisted before create: %+v %v", inv, err)
	}
	s.inv = inv // Simulate a new command after restart.
	p.createErr = nil
	host, err := s.Ensure(context.Background())
	if err != nil || host != "192.168.50.10" || p.creates != 1 || p.starts != 1 {
		t.Fatalf("host=%s err=%v creates=%d starts=%d", host, err, p.creates, p.starts)
	}
	if _, err := s.Ensure(context.Background()); err != nil || p.creates != 1 || p.starts != 1 {
		t.Fatalf("second up mutated VM: %v", err)
	}
	p.machine = nil
	if _, err := s.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "silently recreate") {
		t.Fatalf("lost initialized VM: %v", err)
	}
}

func TestOwnershipAndScopeProtectResources(t *testing.T) {
	s, p := testSession(t)
	p.machine = &provider.Machine{Name: s.spec.Managed.Name, State: "Running"}
	if _, err := s.Ensure(context.Background()); err == nil {
		t.Fatal("adopted an unowned VM")
	}
	if err := s.Destroy(context.Background()); err == nil || p.deletes != 0 {
		t.Fatal("deleted VM without inventory")
	}
	p.machine = nil
	if _, err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.machine.Owner = "someone-else"
	if err := s.Destroy(context.Background()); err == nil || p.deletes != 0 {
		t.Fatal("deleted VM with mismatched ownership")
	}
	p.machine.Owner = s.inv.ClusterID
	p.machine.ID = "replacement-instance"
	if err := s.Destroy(context.Background()); err == nil || p.deletes != 0 {
		t.Fatal("deleted a replacement VM with copied ownership metadata")
	}
	p.machine.ID = s.inv.ResourceID
	s.spec.KubernetesVersion = "v1.34.1+k3s1"
	if _, err := s.Ensure(context.Background()); err != nil {
		t.Fatalf("K3s edit should not recreate infrastructure: %v", err)
	}
	s.spec.Managed.Incus.CPUs++
	if _, err := s.Ensure(context.Background()); err == nil {
		t.Fatal("accepted unsupported hardware edit")
	}
	s.endpoint = "https://different-server"
	if err := s.checkScope(); err == nil {
		t.Fatal("accepted endpoint change")
	}
	s.endpoint = s.inv.Endpoint
	if err := s.Destroy(context.Background()); err != nil || p.deletes != 1 {
		t.Fatalf("destroy=%v count=%d", err, p.deletes)
	}
	if inv, _ := s.store.Load(); inv != nil {
		t.Fatal("inventory remained after successful delete")
	}
}

func TestAddressChangeRequiresExplicitRecovery(t *testing.T) {
	s, p := testSession(t)
	if _, err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.machine.Addresses = []string{"192.168.50.20"}
	if _, err := s.address(p.machine); err == nil {
		t.Fatal("accepted changed DHCP address")
	}
	s.spec.Managed.Network = &cluster.StaticNetwork{Address: "192.168.50.10/24", Gateway: "192.168.50.1"}
	if addr, err := s.address(p.machine); err != nil || addr != "" {
		t.Fatalf("must wait for declared static address, got %q %v", addr, err)
	}
}

func TestAuthorizedKeyDefaultsToAgent(t *testing.T) {
	s, _ := testSession(t)
	s.spec.Managed.AuthorizedKey = ""
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodetest.AgentWith(t, agent.AddedKey{PrivateKey: key, Comment: "me@laptop"})
	if err := s.desired(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.spec.Managed.AuthorizedKey, "ssh-ed25519 ") || !strings.Contains(s.request.UserData, "me@laptop") {
		t.Fatalf("authorized key %q, user data:\n%s", s.spec.Managed.AuthorizedKey, s.request.UserData)
	}

	s.spec.Managed.AuthorizedKey = ""
	nodetest.Agent(t)
	if err := s.desired(); err == nil || !strings.Contains(err.Error(), "test-servers-001") {
		t.Fatalf("expected an error naming the VM, got %v", err)
	}
}

// A VM that never reports an address fails quickly with a hint, rather
// than waiting out the whole provisioning deadline.
func TestMissingAddressFailsFast(t *testing.T) {
	s, p := testSession(t)
	if _, err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.machine.Addresses = nil
	s.addressTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := s.Ensure(context.Background())
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
	for _, want := range []string{"test-servers-001 reported no IPv4 address within 50ms", "guest agent", "images:debian/12/cloud", "DHCP", "kept for inspection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}

	// Cancelling the command is reported as such, not as a broken image.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}

	s.spec.Managed.Network = &cluster.StaticNetwork{Address: "192.168.50.10/24", Gateway: "192.168.50.1"}
	err = s.noAddress(&provider.Machine{Name: "test-servers-001", Addresses: []string{"192.168.50.99"}}, time.Minute)
	if !strings.Contains(err.Error(), "didn't report its address 192.168.50.10/24 (it reports 192.168.50.99)") {
		t.Fatalf("static address error: %v", err)
	}
}
