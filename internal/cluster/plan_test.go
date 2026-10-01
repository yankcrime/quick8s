package cluster

import (
	"reflect"
	"strings"
	"testing"

	"quick8s/internal/k3s"
)

func TestPlan(t *testing.T) {
	spec := &Spec{
		ControlPlanes: []Node{{"cp1"}, {"cp2"}, {"cp3"}},
		Workers:       []Node{{"w1"}, {"w2"}},
	}
	const (
		none   = k3s.NotInstalled
		server = k3s.InstalledServer
		agent  = k3s.InstalledAgent
	)
	for _, tt := range []struct {
		name      string
		installed map[string]k3s.Installation
		want      Plan
		wantErr   string
	}{
		{
			name:      "fresh cluster",
			installed: map[string]k3s.Installation{},
			want:      Plan{Server: "cp1", InitServer: true, JoinControlPlanes: []string{"cp2", "cp3"}, JoinWorkers: []string{"w1", "w2"}},
		},
		{
			name:      "up to date",
			installed: map[string]k3s.Installation{"cp1": server, "cp2": server, "cp3": server, "w1": agent, "w2": agent},
			want:      Plan{Server: "cp1"},
		},
		{
			name:      "new nodes added",
			installed: map[string]k3s.Installation{"cp1": server, "cp2": none, "cp3": server, "w1": agent},
			want:      Plan{Server: "cp1", JoinControlPlanes: []string{"cp2"}, JoinWorkers: []string{"w2"}},
		},
		{
			// A new node listed first must join the existing cluster, not
			// initialize a second one.
			name:      "new node listed first",
			installed: map[string]k3s.Installation{"cp2": server},
			want:      Plan{Server: "cp2", JoinControlPlanes: []string{"cp1", "cp3"}, JoinWorkers: []string{"w1", "w2"}},
		},
		{
			name:      "worker listed as control plane",
			installed: map[string]k3s.Installation{"cp1": server, "cp2": agent},
			wantErr:   "cp2 is listed as a control plane",
		},
		{
			name:      "control plane listed as worker",
			installed: map[string]k3s.Installation{"cp1": server, "w2": server},
			wantErr:   "w2 is listed as a worker",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := spec.Plan(tt.installed)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if got.Empty() != (tt.name == "up to date") {
				t.Fatalf("Empty() = %v", got.Empty())
			}
		})
	}
}
