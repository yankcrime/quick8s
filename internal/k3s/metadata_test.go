package k3s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseTaint(t *testing.T) {
	for in, want := range map[string]Taint{
		"dedicated=gpu:NoSchedule":   {"dedicated", "gpu", "NoSchedule"},
		"example.com/spot:NoExecute": {"example.com/spot", "", "NoExecute"},
	} {
		if got, err := ParseTaint(in); err != nil || got != want {
			t.Errorf("ParseTaint(%q) = %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{"dedicated=gpu", "dedicated:Sometimes", ":NoSchedule", "a b:NoSchedule", "k=v v:NoSchedule"} {
		if _, err := ParseTaint(in); err == nil {
			t.Errorf("ParseTaint(%q) accepted", in)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	for _, ok := range [][2]string{{"tier", "general"}, {"node-role.kubernetes.io/worker", "true"}, {"example.com/empty", ""}} {
		if err := ValidateLabel(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"quick8s.io/pool", "x"}, {"Bad_Prefix/x", "y"}, {"tier", "has space"}, {"", "x"}, {"tier", strings.Repeat("a", 64)}} {
		if err := ValidateLabel(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func node(labels, annotations map[string]string, taints ...map[string]any) nodeObject {
	var n nodeObject
	n.Metadata.ResourceVersion = "42"
	n.Metadata.Labels, n.Metadata.Annotations, n.Spec.Taints = labels, annotations, taints
	return n
}

func TestMetadataPatch(t *testing.T) {
	gpu := Taint{"dedicated", "gpu", "NoSchedule"}
	// Set by Kubernetes itself, and must survive any rewrite of the list.
	unreachable := map[string]any{"key": "node.kubernetes.io/unreachable", "effect": "NoExecute", "timeAdded": "2026-01-01T00:00:00Z"}
	managed := map[string]string{managedLabelsAnnotation: "tier", managedTaintsAnnotation: "dedicated:NoSchedule"}
	for _, tt := range []struct {
		name  string
		node  nodeObject
		want  NodeMetadata
		patch string // JSON, or "" for no change
	}{
		{
			name:  "nothing managed",
			node:  node(map[string]string{"kubernetes.io/hostname": "a"}, nil),
			patch: "",
		},
		{
			name: "first apply",
			node: node(map[string]string{"kubernetes.io/hostname": "a"}, nil, unreachable),
			want: NodeMetadata{Labels: map[string]string{"tier": "general"}, Taints: []Taint{gpu}},
			patch: `{"metadata":{"annotations":{"quick8s.io/managed-labels":"tier","quick8s.io/managed-taints":"dedicated:NoSchedule"},"labels":{"tier":"general"},"resourceVersion":"42"},` +
				`"spec":{"taints":[{"effect":"NoExecute","key":"node.kubernetes.io/unreachable","timeAdded":"2026-01-01T00:00:00Z"},{"effect":"NoSchedule","key":"dedicated","value":"gpu"}]}}`,
		},
		{
			name:  "converged",
			node:  node(map[string]string{"tier": "general"}, managed, map[string]any{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}),
			want:  NodeMetadata{Labels: map[string]string{"tier": "general"}, Taints: []Taint{gpu}},
			patch: "",
		},
		{
			name:  "changed value",
			node:  node(map[string]string{"tier": "general"}, managed, map[string]any{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}),
			want:  NodeMetadata{Labels: map[string]string{"tier": "batch"}, Taints: []Taint{{"dedicated", "batch", "NoSchedule"}}},
			patch: `{"metadata":{"labels":{"tier":"batch"},"resourceVersion":"42"},"spec":{"taints":[{"effect":"NoSchedule","key":"dedicated","value":"batch"}]}}`,
		},
		{
			// Only what quick8s set is removed; other labels and taints stay.
			name: "dropped from the definition",
			node: node(map[string]string{"tier": "general", "team": "x"}, managed, map[string]any{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}, unreachable),
			patch: `{"metadata":{"annotations":{"quick8s.io/managed-labels":null,"quick8s.io/managed-taints":null},"labels":{"tier":null},"resourceVersion":"42"},` +
				`"spec":{"taints":[{"effect":"NoExecute","key":"node.kubernetes.io/unreachable","timeAdded":"2026-01-01T00:00:00Z"}]}}`,
		},
		{
			// A label already present from registration is adopted, not rewritten.
			name:  "adopt existing",
			node:  node(map[string]string{"tier": "general"}, nil),
			want:  NodeMetadata{Labels: map[string]string{"tier": "general"}},
			patch: `{"metadata":{"annotations":{"quick8s.io/managed-labels":"tier"},"resourceVersion":"42"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			patch, changed := metadataPatch(tt.node, tt.want)
			if changed != (tt.patch != "") {
				t.Fatalf("changed = %v, patch %v", changed, patch)
			}
			if !changed {
				return
			}
			got, _ := json.Marshal(patch)
			if string(got) != tt.patch {
				t.Fatalf("patch\n got %s\nwant %s", got, tt.patch)
			}
		})
	}
}

func TestApplyNodeMetadataWaitsForRegistration(t *testing.T) {
	cert := kubeletCertificate(t, "system:node:worker-1")
	n := remoteStub{runRoot: func(cmd string) (string, error) { return cert, nil }}
	gets := 0
	var patched string
	server := remoteStub{runRoot: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, " get node 'worker-1' -o json"):
			if gets++; gets < 3 {
				return "", errors.New(`nodes "worker-1" not found`)
			}
			return `{"metadata":{"resourceVersion":"7","labels":{}}}`, nil
		case strings.Contains(cmd, " patch node 'worker-1' --type=merge -p "):
			patched = cmd
			return "", nil
		}
		t.Fatalf("unexpected command %q", cmd)
		return "", nil
	}}
	name, changed, err := applyNodeMetadata(context.Background(), n, server, NodeMetadata{Labels: map[string]string{"tier": "gpu"}}, time.Millisecond)
	if err != nil || name != "worker-1" || !changed || gets != 3 || !strings.Contains(patched, `"tier":"gpu"`) {
		t.Fatalf("name=%s changed=%v err=%v gets=%d patch=%s", name, changed, err, gets, patched)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	missing := remoteStub{runRoot: func(string) (string, error) { return "", errors.New("not found") }}
	if _, _, err := applyNodeMetadata(ctx, n, missing, NodeMetadata{}, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a bounded wait, got %v", err)
	}
}

func TestApplyNodeMetadataRetriesConflicts(t *testing.T) {
	cert := kubeletCertificate(t, "system:node:server-1")
	n := remoteStub{runRoot: func(string) (string, error) { return cert, nil }}
	version, patches := 1, []string{}
	server := remoteStub{runRoot: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, " get node "):
			return fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"}}`, version), nil
		case strings.Contains(cmd, " patch node "):
			patches = append(patches, cmd)
			if len(patches) == 1 {
				version++ // K3s updated the node in between.
				return "", errors.New(`Error from server (Conflict): Operation cannot be fulfilled on nodes "server-1": the object has been modified`)
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected command %q", cmd)
	}}
	_, changed, err := applyNodeMetadata(context.Background(), n, server, NodeMetadata{Labels: map[string]string{"tier": "a"}}, time.Millisecond)
	if err != nil || !changed || len(patches) != 2 || !strings.Contains(patches[1], `"resourceVersion":"2"`) {
		t.Fatalf("changed=%v err=%v patches=%q", changed, err, patches)
	}

	// Other errors aren't retried.
	patches = nil
	failing := remoteStub{runRoot: func(cmd string) (string, error) {
		if strings.Contains(cmd, " patch node ") {
			patches = append(patches, cmd)
			return "", errors.New("Error from server (Forbidden)")
		}
		return `{"metadata":{"resourceVersion":"1"}}`, nil
	}}
	if _, _, err := applyNodeMetadata(context.Background(), n, failing, NodeMetadata{Labels: map[string]string{"tier": "a"}}, time.Millisecond); err == nil || len(patches) != 1 {
		t.Fatalf("err=%v patches=%d", err, len(patches))
	}
}
