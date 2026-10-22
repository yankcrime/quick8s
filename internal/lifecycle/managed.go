// Package lifecycle coordinates managed infrastructure before K3s reconciliation.
package lifecycle

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"quick8s/internal/cluster"
	"quick8s/internal/node"
	"quick8s/internal/provider"
	incus "quick8s/internal/provider/incus"
	"quick8s/internal/state"
)

type Session struct {
	provider      provider.Provider
	closeProvider func()
	store         *state.Store
	spec          *cluster.Spec
	inv           *state.Inventory
	endpoint      string
	request       provider.Request
	progress      func(string)
	// addressTimeout bounds the wait for a started VM's address, or
	// defaultAddressTimeout when zero; tests shorten it.
	addressTimeout time.Duration
}

// defaultAddressTimeout is how long a running VM gets to report its
// address. A cloud image on a working network takes well under a minute,
// so a VM still without one is broken rather than slow: failing here,
// with a hint, beats waiting out the whole provisioning deadline.
const defaultAddressTimeout = 3 * time.Minute

// Open locks identity for mutating commands. It does not create a VM or an inventory.
func Open(specPath string, spec *cluster.Spec, progress func(string)) (*Session, error) {
	store, err := state.Open(specPath)
	if err != nil {
		return nil, err
	}
	p, err := incus.Connect(spec.Managed.Incus)
	if err != nil {
		store.Close()
		return nil, err
	}
	inv, err := store.Load()
	if err != nil {
		p.Close()
		store.Close()
		return nil, err
	}
	s := &Session{provider: p, closeProvider: p.Close, store: store, spec: spec, inv: inv, endpoint: p.Endpoint, progress: progress}
	if err := s.checkScope(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) Close() {
	if s.closeProvider != nil {
		s.closeProvider()
	}
	if s.store != nil {
		s.store.Close()
	}
}

func (s *Session) checkScope() error {
	if s.inv == nil {
		return nil
	}
	i := s.spec.Managed.Incus
	if s.inv.ClusterName != s.spec.Name || s.inv.Remote != i.Remote || s.inv.Project != i.Project || s.inv.Endpoint != s.endpoint {
		return errors.New("cluster/provider identity differs from the inventory; restore the original definition and remote before proceeding")
	}
	return nil
}

func (s *Session) desired() error {
	if m := s.spec.Managed; m.AuthorizedKey == "" {
		key, err := node.AgentKey()
		if err != nil {
			return fmt.Errorf("choosing the key to authorize on %s: %w", m.Name, err)
		}
		m.AuthorizedKey = key
	}
	// Stable, locally administered MAC scoped to the server, project and VM.
	// Guest interface names are not controlled by Incus's NIC device name.
	macHash := sha256.Sum256([]byte(s.endpoint + "/" + s.spec.Managed.Incus.Project + "/" + s.spec.Managed.Name))
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", macHash[0], macHash[1], macHash[2], macHash[3], macHash[4])
	user, network, err := s.spec.GuestData(mac)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Machine       cluster.Incus
		User, Network string
		Port          int
	}{s.spec.Managed.Incus, user, network, s.spec.SSH.Port})
	if err != nil {
		return err
	}
	s.request = provider.Request{Name: s.spec.Managed.Name, ConfigHash: fmt.Sprintf("%x", sha256.Sum256(data)), UserData: user, NetworkData: network, MACAddress: mac}
	if s.inv != nil && (s.inv.Machine != s.request.Name || s.inv.ConfigHash != s.request.ConfigHash) {
		return errors.New("machine or cloud-init configuration changed; MVP requires explicit down/recreation with the original definition")
	}
	return nil
}

func owned(m *provider.Machine, inv *state.Inventory) error {
	if m == nil {
		return nil
	}
	if inv == nil || m.Name != inv.Machine || m.Owner != inv.ClusterID || m.ConfigHash != inv.ConfigHash || inv.ResourceID != "" && m.ID != inv.ResourceID {
		return fmt.Errorf("refusing to use %s: ownership/configuration does not match the inventory; restore its state file if lost", m.Name)
	}
	return nil
}

func (s *Session) Ensure(ctx context.Context) (string, error) {
	if err := s.desired(); err != nil {
		return "", err
	}
	if err := s.provider.Validate(ctx); err != nil {
		return "", err
	}
	m, err := s.provider.Inspect(ctx, s.request.Name)
	if err != nil {
		return "", err
	}
	if err := owned(m, s.inv); err != nil {
		return "", err
	}
	if s.inv == nil {
		s.inv = &state.Inventory{Version: 1, ClusterID: rand.Text(), ClusterName: s.spec.Name,
			Remote: s.spec.Managed.Incus.Remote, Endpoint: s.endpoint, Project: s.spec.Managed.Incus.Project,
			Machine: s.request.Name, ConfigHash: s.request.ConfigHash}
		if err := s.store.Save(s.inv); err != nil {
			return "", err
		}
	}
	if m == nil {
		if s.inv.Provisioned {
			return "", errors.New("previously provisioned VM is missing; refusing to silently recreate an empty Kubernetes cluster")
		}
		s.request.Owner = s.inv.ClusterID
		s.progress("Creating Incus VM " + s.request.Name + "...")
		if err := s.provider.Create(ctx, s.request); err != nil {
			return "", fmt.Errorf("creating VM (preserved for retry): %w", err)
		}
		m, err = s.provider.Inspect(ctx, s.request.Name)
		if err != nil {
			return "", err
		}
		if m == nil {
			return "", errors.New("created VM could not be found; rerun up to inspect it")
		}
	}
	if err := owned(m, s.inv); err != nil {
		return "", err
	}
	s.request.Owner = s.inv.ClusterID
	if err := s.provider.Check(ctx, s.request); err != nil {
		return "", err
	}
	s.inv.Provisioned, s.inv.Image, s.inv.ResourceID = true, m.Image, m.ID
	if err := s.store.Save(s.inv); err != nil {
		return "", err
	}
	switch m.State {
	case "Stopped":
		s.progress("Starting " + m.Name + "...")
		if err := s.provider.Start(ctx, m.Name); err != nil {
			return "", err
		}
	case "Running":
	default:
		return "", fmt.Errorf("VM %s is %s; wait for its Incus operation to finish, then retry", m.Name, m.State)
	}
	s.progress("Waiting for VM address...")
	timeout := cmp.Or(s.addressTimeout, defaultAddressTimeout)
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		m, err = s.provider.Inspect(ctx, s.inv.Machine)
		if err != nil {
			return "", err
		}
		if m == nil {
			return "", errors.New("VM disappeared while waiting for an address")
		}
		if err := owned(m, s.inv); err != nil {
			return "", err
		}
		address, err := s.address(m)
		if err != nil {
			return "", err
		}
		if address != "" {
			s.inv.Address = address
			if err := s.store.Save(s.inv); err != nil {
				return "", err
			}
			return address, nil
		}
		if err := pause(actx); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("waiting for VM address: %w", ctx.Err())
			}
			return "", s.noAddress(m, timeout)
		}
	}
}

// noAddress explains a VM that never reported a usable address. Addresses
// come from the Incus guest agent, which cloud images ship and start, so
// a missing agent is the commonest cause; the network is the other.
func (s *Session) noAddress(m *provider.Machine, timeout time.Duration) error {
	i := s.spec.Managed.Incus
	what := "reported no IPv4 address"
	cause := fmt.Sprintf("that network %s hands out DHCP leases", i.Network)
	if n := s.spec.Managed.Network; n != nil {
		what = "didn't report its address " + n.Address
		cause = fmt.Sprintf("that the address is valid on network %s", i.Network)
	}
	if len(m.Addresses) > 0 {
		what += fmt.Sprintf(" (it reports %s)", strings.Join(m.Addresses, ", "))
	}
	return fmt.Errorf("VM %s %s within %s. Check that image %s includes the Incus guest agent and cloud-init "+
		"(on images:, use a /cloud variant), and %s. The VM is kept for inspection; `quick8s down` removes it",
		m.Name, what, timeout, i.Image, cause)
}

func (s *Session) address(m *provider.Machine) (string, error) {
	if n := s.spec.Managed.Network; n != nil {
		prefix, _ := netip.ParsePrefix(n.Address)
		ip := prefix.Addr().String()
		if slices.Contains(m.Addresses, ip) {
			return ip, nil
		}
		return "", nil
	}
	if s.inv.Address != "" {
		if slices.Contains(m.Addresses, s.inv.Address) {
			return s.inv.Address, nil
		}
		if len(m.Addresses) > 0 {
			return "", errors.New("VM address changed; refusing to change the Kubernetes node IP automatically; use an explicit static address")
		}
		return "", nil
	}
	if len(m.Addresses) > 1 {
		return "", errors.New("VM has multiple IPv4 addresses; configure an explicit member address")
	}
	if len(m.Addresses) == 1 {
		return m.Addresses[0], nil
	}
	return "", nil
}

// WaitGuest returns the same SSH connection used for cloud-init and the K3s rollout.
func WaitGuest(ctx context.Context, target node.Target, progress func(string)) (*node.Client, error) {
	progress("Waiting for SSH at " + target.Host + "...")
	c, err := dialGuest(ctx, target)
	if err != nil {
		return nil, err
	}
	progress("Waiting for cloud-init completion...")
	stop := context.AfterFunc(ctx, func() { c.Close() })
	// timeout bounds the remote wait as well as this command's context.
	_, err = c.RunAsRoot("timeout 300 cloud-init status --wait")
	stop()
	if err := cloudInitResult(err, progress); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// dialGuest retries until sshd answers. sshd can accept connections and
// still fail handshakes while cloud-init regenerates its host keys, so any
// failure is retried, except authentication: cloud-init creates the guest
// account before sshd starts, so a rejected key, or no agent, is final.
func dialGuest(ctx context.Context, target node.Target) (*node.Client, error) {
	var lastErr error
	for {
		c, err := node.Dial(target)
		if err == nil {
			return c, nil
		}
		if errors.Is(err, node.ErrAuth) {
			return nil, err
		}
		lastErr = err
		if err := pause(ctx); err != nil {
			return nil, fmt.Errorf("waiting for SSH: %w (last check: %v)", err, lastErr)
		}
	}
}

// cloudInitResult accepts exit status 2, which cloud-init uses when it
// finished with recoverable errors, including deprecation warnings about
// keys in the user's own cloud-config.
func cloudInitResult(err error, progress func(string)) error {
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) && exit.ExitStatus() == 2 {
		progress("cloud-init finished with recoverable errors; see `cloud-init status --long` on the VM")
		return nil
	}
	if err != nil {
		return fmt.Errorf("cloud-init did not complete successfully: %w", err)
	}
	return nil
}

func pause(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Destroy only removes the resource in the inventory, after checking remote ownership.
func (s *Session) Destroy(ctx context.Context) error {
	if s.inv == nil {
		return errors.New("no managed inventory; refusing to infer ownership from a VM name")
	}
	m, err := s.provider.Inspect(ctx, s.inv.Machine)
	if err != nil {
		return err
	}
	if err := owned(m, s.inv); err != nil {
		return err
	}
	if m != nil {
		s.progress("Deleting Incus VM and root disk " + m.Name + "...")
		if err := s.provider.Delete(ctx, m.Name); err != nil {
			return err
		}
	}
	return s.store.Remove()
}

func (s *Session) MachineName() string {
	if s.inv != nil {
		return s.inv.Machine
	}
	return s.spec.Managed.Name
}

// NeedsBootstrap also detects an interrupted initial installer that left a
// binary/uninstall script but never brought up a healthy cluster.
func (s *Session) NeedsBootstrap() bool { return s.inv == nil || !s.inv.K3sReady }

func (s *Session) MarkReady() error {
	s.inv.K3sReady = true
	return s.store.Save(s.inv)
}

// Describe is read-only and does not create a state directory or inventory.
func Describe(ctx context.Context, path string, spec *cluster.Spec) (string, error) {
	p, err := incus.Connect(spec.Managed.Incus)
	if err != nil {
		return "", err
	}
	defer p.Close()
	inv, err := state.Read(state.Path(path))
	if err != nil {
		return "", err
	}
	s := &Session{provider: p, spec: spec, inv: inv, endpoint: p.Endpoint}
	if err := s.checkScope(); err != nil {
		return "", err
	}
	if err := s.desired(); err != nil {
		return "", err
	}
	m, err := p.Inspect(ctx, spec.Managed.Name)
	if err != nil {
		return "", err
	}
	if err := owned(m, inv); err != nil {
		return "", err
	}
	if m == nil {
		if inv != nil && inv.Provisioned {
			return "", errors.New("previously provisioned VM is missing; explicit recovery required")
		}
		return fmt.Sprintf("%s: create Incus VM, initialize guest, then reconcile K3s", spec.Managed.Name), nil
	}
	return fmt.Sprintf("%s: %s; addresses: %v; K3s will be probed over SSH by up", m.Name, m.State, m.Addresses), nil
}
