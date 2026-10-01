package cluster

import (
	"fmt"

	"quick8s/internal/k3s"
)

// Plan is what `up` has to do to bring the nodes in line with a Spec. It
// only ever adds: nodes that already run K3s in their declared role are
// left alone.
type Plan struct {
	// Server is the control plane other nodes join, and the address the
	// kubeconfig points at: the first control plane already running K3s,
	// or else the first one listed, which then initializes the cluster.
	Server string
	// InitServer means Server isn't installed yet, so installing it
	// creates the cluster.
	InitServer        bool
	JoinControlPlanes []string
	JoinWorkers       []string
}

// Empty reports whether every node already matches the spec.
func (p Plan) Empty() bool {
	return !p.InitServer && len(p.JoinControlPlanes) == 0 && len(p.JoinWorkers) == 0
}

// Plan compares the spec against what each node already has installed,
// keyed by host.
func (s *Spec) Plan(installed map[string]k3s.Installation) (Plan, error) {
	var p Plan
	var pending []string
	for _, n := range s.ControlPlanes {
		switch installed[n.Host] {
		case k3s.InstalledServer:
			if p.Server == "" {
				p.Server = n.Host
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
		switch installed[n.Host] {
		case k3s.InstalledAgent:
		case k3s.InstalledServer:
			return Plan{}, fmt.Errorf("%s is listed as a worker, but K3s is installed there as a server (control plane); tear it down first", n.Host)
		default:
			p.JoinWorkers = append(p.JoinWorkers, n.Host)
		}
	}
	return p, nil
}

// Nodes returns every node, control planes first.
func (s *Spec) Nodes() []Node {
	return append(append([]Node(nil), s.ControlPlanes...), s.Workers...)
}
