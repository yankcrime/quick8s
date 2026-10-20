package cluster

import (
	"reflect"
	"strings"
	"testing"

	"quick8s/internal/k3s"
)

func TestPlan(t *testing.T) {
	const (
		v133 = "v1.33.4+k3s1"
		v134 = "v1.34.1+k3s1"
	)
	serverConfig := []byte("disable:\n    - traefik\n")
	spec := &Spec{
		ControlPlanes: []Node{{"cp1"}, {"cp2"}, {"cp3"}},
		Workers:       []Node{{"w1"}, {"w2"}},
		ServerConfig:  serverConfig,
	}
	at := func(version string) Spec {
		s := *spec
		s.KubernetesVersion = version
		return s
	}
	server := func(version string) NodeState {
		return NodeState{Installation: k3s.InstalledServer, Version: version, Config: serverConfig}
	}
	agent := func(version string) NodeState {
		return NodeState{Installation: k3s.InstalledAgent, Version: version}
	}
	upToDate := map[string]NodeState{"cp1": server(v133), "cp2": server(v133), "cp3": server(v133), "w1": agent(v133), "w2": agent(v133)}
	with := func(changes map[string]NodeState) map[string]NodeState {
		out := map[string]NodeState{}
		for k, v := range upToDate {
			out[k] = v
		}
		for k, v := range changes {
			out[k] = v
		}
		return out
	}

	for _, tt := range []struct {
		name    string
		spec    Spec
		nodes   map[string]NodeState
		want    Plan
		wantErr string
	}{
		{
			name:  "fresh cluster",
			spec:  at(v134),
			nodes: map[string]NodeState{},
			want:  Plan{Server: "cp1", InitServer: true, JoinControlPlanes: []string{"cp2", "cp3"}, JoinWorkers: []string{"w1", "w2"}},
		},
		{
			name:  "up to date",
			spec:  at(v133),
			nodes: upToDate,
			want:  Plan{Server: "cp1"},
		},
		{
			// No kubernetesVersion means never upgrade, whatever's running.
			name:  "no version declared",
			spec:  at(""),
			nodes: with(map[string]NodeState{"w1": agent("v1.32.1+k3s1")}),
			want:  Plan{Server: "cp1"},
		},
		{
			name:  "new nodes added",
			spec:  at(v133),
			nodes: map[string]NodeState{"cp1": server(v133), "cp3": server(v133), "w1": agent(v133)},
			want:  Plan{Server: "cp1", JoinControlPlanes: []string{"cp2"}, JoinWorkers: []string{"w2"}},
		},
		{
			// A new node listed first must join the existing cluster, not
			// initialize a second one.
			name:  "new node listed first",
			spec:  at(v133),
			nodes: map[string]NodeState{"cp2": server(v133)},
			want:  Plan{Server: "cp2", JoinControlPlanes: []string{"cp1", "cp3"}, JoinWorkers: []string{"w1", "w2"}},
		},
		{
			name:  "upgrade, control planes first",
			spec:  at(v134),
			nodes: upToDate,
			want: Plan{Server: "cp1", Update: []Update{
				{Host: "cp1", ControlPlane: true, From: v133, To: v134},
				{Host: "cp2", ControlPlane: true, From: v133, To: v134},
				{Host: "cp3", ControlPlane: true, From: v133, To: v134},
				{Host: "w1", From: v133, To: v134},
				{Host: "w2", From: v133, To: v134},
			}},
		},
		{
			// An interrupted upgrade picks up where it left off, and new
			// nodes join after it.
			name:  "resume upgrade and add node",
			spec:  at(v134),
			nodes: map[string]NodeState{"cp1": server(v134), "cp2": server(v133), "cp3": server(v134), "w1": agent(v134)},
			want: Plan{
				Server:      "cp1",
				Update:      []Update{{Host: "cp2", ControlPlane: true, From: v133, To: v134}},
				JoinWorkers: []string{"w2"},
			},
		},
		{
			name: "config changed",
			spec: at(v133),
			nodes: with(map[string]NodeState{
				"cp2": {Installation: k3s.InstalledServer, Version: v133, Config: []byte("disable: [traefik, metrics-server]\n")},
				"w2":  {Installation: k3s.InstalledAgent, Version: v133, Config: []byte("node-label: [a=b]\n")},
			}),
			want: Plan{Server: "cp1", Update: []Update{
				{Host: "cp2", ControlPlane: true, Config: true},
				{Host: "w2", Config: true},
			}},
		},
		{
			// Formatting and comments aren't a change worth a restart.
			name:  "config reformatted",
			spec:  at(v133),
			nodes: with(map[string]NodeState{"cp3": {Installation: k3s.InstalledServer, Version: v133, Config: []byte("# managed\ndisable: [traefik]\n")}}),
			want:  Plan{Server: "cp1"},
		},
		{
			name:  "upgrade and config changed",
			spec:  at(v134),
			nodes: with(map[string]NodeState{"cp1": {Installation: k3s.InstalledServer, Version: v134}, "cp2": {Installation: k3s.InstalledServer, Version: v133}}),
			want: Plan{Server: "cp1", Update: []Update{
				{Host: "cp1", ControlPlane: true, Config: true},
				{Host: "cp2", ControlPlane: true, From: v133, To: v134, Config: true},
				{Host: "cp3", ControlPlane: true, From: v133, To: v134},
				{Host: "w1", From: v133, To: v134},
				{Host: "w2", From: v133, To: v134},
			}},
		},
		{
			name: "retry config restart after file was replaced",
			spec: at(v133),
			nodes: with(map[string]NodeState{
				"cp1": {Installation: k3s.InstalledServer, Version: v133, Config: serverConfig, Pending: &k3s.PendingUpdate{}},
			}),
			want: Plan{Server: "cp1", Update: []Update{{Host: "cp1", ControlPlane: true, Restart: true}}},
		},
		{
			name: "retry upgrade after binary was replaced",
			spec: at(v134),
			nodes: map[string]NodeState{
				"cp1": {Installation: k3s.InstalledServer, Version: v134, Config: serverConfig, Pending: &k3s.PendingUpdate{Version: v134}},
				"cp2": server(v134), "cp3": server(v134), "w1": agent(v134), "w2": agent(v134),
			},
			want: Plan{Server: "cp1", Update: []Update{{Host: "cp1", ControlPlane: true, From: v134, To: v134, Restart: true}}},
		},
		{
			name: "finish pending upgrade when version omitted",
			spec: at(""),
			nodes: with(map[string]NodeState{
				"cp1": {Installation: k3s.InstalledServer, Version: v133, Config: serverConfig, Pending: &k3s.PendingUpdate{Version: v134}},
			}),
			want: Plan{Server: "cp1", Update: []Update{{Host: "cp1", ControlPlane: true, From: v133, To: v134, Restart: true}}},
		},
		{
			name:    "downgrade",
			spec:    at(v133),
			nodes:   with(map[string]NodeState{"w2": agent(v134)}),
			wantErr: "w2: K3s v1.34.1+k3s1 is installed, which is newer than v1.33.4+k3s1",
		},
		{
			name:    "skips a minor version",
			spec:    at("v1.35.0+k3s1"),
			nodes:   upToDate,
			wantErr: "cp1: upgrading K3s v1.33.4+k3s1 to v1.35.0+k3s1 would skip a minor version",
		},
		{
			name:    "worker listed as control plane",
			spec:    at(v133),
			nodes:   map[string]NodeState{"cp1": server(v133), "cp2": agent(v133)},
			wantErr: "cp2 is listed as a control plane",
		},
		{
			name:    "control plane listed as worker",
			spec:    at(v133),
			nodes:   map[string]NodeState{"cp1": server(v133), "w2": server(v133)},
			wantErr: "w2 is listed as a worker",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.Plan(tt.nodes)
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
			if wantEmpty := reflect.DeepEqual(tt.want, Plan{Server: "cp1"}); got.Empty() != wantEmpty {
				t.Fatalf("Empty() = %v", got.Empty())
			}
		})
	}
}

func TestSameConfig(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"disable: [traefik]\n", "disable:\n  - traefik # not needed\n", true},
		{"a: 1\nb: 2\n", "b: 2\na: 1\n", true},
		{"disable: [traefik]\n", "", false},
		{"node-label: [a=b]\n", "node-label: [a=c]\n", false},
		{"a: 1\n", "a: [unterminated\n", false},
	} {
		if got := sameConfig([]byte(tt.a), []byte(tt.b)); got != tt.want {
			t.Errorf("sameConfig(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
	if !sameConfig(nil, []byte{}) {
		t.Error("a missing config file should match an absent k3s section")
	}
}
