package cluster

import (
	"bytes"
	"fmt"
	"reflect"

	"go.yaml.in/yaml/v3"

	"quick8s/internal/k3s"
)

// NodeState is what `up` finds on a node before planning.
type NodeState struct {
	Installation k3s.Installation
	// Version is the installed K3s release, when K3s is installed.
	Version string
	// Config is the node's K3s config file, or nil when it has none.
	Config []byte
	// Pending is an update that has not yet passed its readiness check.
	Pending *k3s.PendingUpdate
}

// Plan is what `up` has to do to bring the nodes in line with a Spec.
// Nodes not running K3s are added; nodes running it in their declared role
// are upgraded or reconfigured in place where they differ from the spec.
type Plan struct {
	// Server is the control plane other nodes join, and the address the
	// kubeconfig points at: the first control plane already running K3s,
	// or else the first one listed, which then initializes the cluster.
	Server string
	// InitServer means Server isn't installed yet, so installing it
	// creates the cluster.
	InitServer bool
	// Update lists nodes already running K3s that differ from the spec,
	// control planes first. They're changed one at a time, before any node
	// joins, so new nodes join a cluster that's already up to date.
	Update            []Update
	JoinControlPlanes []string
	JoinWorkers       []string
}

// Update is an in-place change to a node already running K3s. Either kind
// of change restarts K3s on the node.
type Update struct {
	Host         string
	ControlPlane bool
	// From and To are the installed and target K3s releases when the
	// installer must run, and empty otherwise. They can match on a retry.
	From, To string
	// Config means the node's K3s config file doesn't match its role's
	// section of the spec, and is replaced by it - or removed, when the
	// spec has none.
	Config bool
	// Restart retries an interrupted update even when the files match.
	Restart bool
}

// Upgrade reports whether the update must run the K3s installer.
func (u Update) Upgrade() bool {
	return u.To != ""
}

// Empty reports whether every node already matches the spec.
func (p Plan) Empty() bool {
	return !p.InitServer && len(p.Update) == 0 && len(p.JoinControlPlanes) == 0 && len(p.JoinWorkers) == 0
}

// Plan compares the spec against what each node has, keyed by host. Every
// upgrade is validated here, before anything changes: a node can't be
// downgraded, or skip a minor version.
func (s *Spec) Plan(nodes map[string]NodeState) (Plan, error) {
	var p Plan
	var pending []string
	for _, n := range s.ControlPlanes {
		switch state := nodes[n.Host]; state.Installation {
		case k3s.InstalledServer:
			if p.Server == "" {
				p.Server = n.Host
			}
			u, err := s.update(n.Host, true, state)
			if err != nil {
				return Plan{}, err
			}
			if u != nil {
				p.Update = append(p.Update, *u)
			}
		case k3s.InstalledAgent:
			return Plan{}, fmt.Errorf("%s is listed as a control plane, but K3s is installed there as an agent (worker); tear it down first", n.Host)
		default:
			pending = append(pending, n.Host)
		}
	}
	if p.Server == "" {
		p.Server, p.InitServer = pending[0], true
		pending = pending[1:]
	}
	p.JoinControlPlanes = pending

	for _, n := range s.Workers {
		switch state := nodes[n.Host]; state.Installation {
		case k3s.InstalledAgent:
			u, err := s.update(n.Host, false, state)
			if err != nil {
				return Plan{}, err
			}
			if u != nil {
				p.Update = append(p.Update, *u)
			}
		case k3s.InstalledServer:
			return Plan{}, fmt.Errorf("%s is listed as a worker, but K3s is installed there as a server (control plane); tear it down first", n.Host)
		default:
			p.JoinWorkers = append(p.JoinWorkers, n.Host)
		}
	}
	return p, nil
}

// update works out how a node already running K3s differs from the spec,
// returning nil if it doesn't.
func (s *Spec) update(host string, controlPlane bool, state NodeState) (*Update, error) {
	u := Update{Host: host, ControlPlane: controlPlane}
	to := s.KubernetesVersion
	if state.Pending != nil {
		u.Restart = true
		if to == "" {
			to = state.Pending.Version
		}
	}
	if to != "" {
		upgrade, err := k3s.CheckUpgrade(state.Version, to)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", host, err)
		}
		if upgrade || state.Pending != nil && state.Pending.Version != "" {
			u.From, u.To = state.Version, to
		}
	}
	u.Config = !sameConfig(s.Config(controlPlane), state.Config)
	if !u.Upgrade() && !u.Config && !u.Restart {
		return nil, nil
	}
	return &u, nil
}

// sameConfig reports whether two K3s config files say the same thing,
// ignoring formatting and comments, so that editing a comment in
// quick8s.yaml doesn't restart every node. Missing and empty are the same.
func sameConfig(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var av, bv any
	if yaml.Unmarshal(a, &av) != nil || yaml.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// Nodes returns every node, control planes first.
func (s *Spec) Nodes() []Node {
	return append(append([]Node(nil), s.ControlPlanes...), s.Workers...)
}
