package k3s

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"quick8s/internal/shell"
)

// K3s's node-label and node-taint only apply when a node first registers,
// and the kubelet refuses most kubernetes.io labels, so quick8s sets node
// labels and taints through the API instead, and keeps them converged.
const (
	// The keys quick8s set last time, so dropping one from the definition
	// removes it from the node, while labels and taints set by anything
	// else are left alone.
	managedLabelsAnnotation = "quick8s.io/managed-labels"
	managedTaintsAnnotation = "quick8s.io/managed-taints"
	reservedPrefix          = "quick8s.io/"
)

// NodeMetadata is the labels and taints quick8s manages on a node.
type NodeMetadata struct {
	Labels map[string]string
	Taints []Taint
}

// Taint is a Kubernetes node taint. Key and Effect identify it.
type Taint struct {
	Key, Value, Effect string
}

func (t Taint) id() string { return t.Key + ":" + t.Effect }

var (
	labelName  = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	dnsDomain  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	taintValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

// ValidateLabel checks a label against Kubernetes' syntax, and rejects
// quick8s's own keys.
func ValidateLabel(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if strings.HasPrefix(key, reservedPrefix) {
		return fmt.Errorf("label %s: the %s prefix is reserved for quick8s", key, reservedPrefix)
	}
	if !taintValue.MatchString(value) {
		return fmt.Errorf("label %s: value %q must be at most 63 alphanumerics, '-', '_' or '.'", key, value)
	}
	return nil
}

func validateKey(key string) error {
	prefix, name, found := strings.Cut(key, "/")
	if !found {
		prefix, name = "", key
	}
	if found && (len(prefix) > 253 || !dnsDomain.MatchString(prefix)) || !labelName.MatchString(name) {
		return fmt.Errorf("%q is not a valid Kubernetes key, e.g. tier or example.com/tier", key)
	}
	return nil
}

// ParseTaint reads a taint in kubectl's key[=value]:Effect form.
func ParseTaint(s string) (Taint, error) {
	rest, effect, ok := strings.Cut(s, ":")
	key, value, _ := strings.Cut(rest, "=")
	t := Taint{Key: key, Value: value, Effect: effect}
	if !ok || !slices.Contains([]string{"NoSchedule", "PreferNoSchedule", "NoExecute"}, effect) {
		return t, fmt.Errorf("taint %q must be key[=value]:Effect, with Effect NoSchedule, PreferNoSchedule or NoExecute", s)
	}
	if err := validateKey(key); err != nil {
		return t, fmt.Errorf("taint %q: %w", s, err)
	}
	if !taintValue.MatchString(value) {
		return t, fmt.Errorf("taint %q: value must be at most 63 alphanumerics, '-', '_' or '.'", s)
	}
	return t, nil
}

// NodeName reads a node's Kubernetes name from its kubelet certificate,
// which reflects --node-name and --with-node-id overrides. Servers run a
// kubelet too, so this works for either role.
func NodeName(c rootRunner) (string, error) {
	out, err := c.RunAsRoot("cat " + kubeletCertPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode([]byte(out))
	if block == nil {
		return "", fmt.Errorf("reading kubelet client certificate: no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("reading kubelet client certificate: %w", err)
	}
	name, ok := strings.CutPrefix(cert.Subject.CommonName, "system:node:")
	if !ok || name == "" {
		return "", fmt.Errorf("unexpected kubelet identity %q", cert.Subject.CommonName)
	}
	return name, nil
}

// ApplyNodeMetadata converges a node's quick8s-managed labels and taints
// through the server's API, waiting for a newly joined node to register.
// It returns the node's name, and whether anything changed.
func ApplyNodeMetadata(ctx context.Context, n, server rootRunner, want NodeMetadata) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	return applyNodeMetadata(ctx, n, server, want, readinessInterval)
}

func applyNodeMetadata(ctx context.Context, n, server rootRunner, want NodeMetadata, interval time.Duration) (string, bool, error) {
	kubectl := binaryPath + " kubectl --kubeconfig " + kubeconfigPath + " --request-timeout=10s "
	var name string
	var lastErr error
	get := func() (nodeObject, error) {
		var current nodeObject
		out, err := server.RunAsRoot(kubectl + "get node " + shell.Quote(name) + " -o json")
		if err != nil {
			return current, err
		}
		if err := json.Unmarshal([]byte(out), &current); err != nil {
			return current, fmt.Errorf("decoding node %s: %w", name, err)
		}
		return current, nil
	}
	var current nodeObject
	for {
		if name, lastErr = NodeName(n); lastErr == nil {
			if current, lastErr = get(); lastErr == nil {
				break
			}
		}
		if err := pause(ctx, interval); err != nil {
			return name, false, fmt.Errorf("waiting for node to register: %w (last check: %v)", err, lastErr)
		}
	}
	for attempt := 1; ; attempt++ {
		patch, changed := metadataPatch(current, want)
		if !changed {
			return name, false, nil
		}
		data, err := json.Marshal(patch)
		if err != nil {
			return name, false, err
		}
		_, err = server.RunAsRoot(kubectl + "patch node " + shell.Quote(name) + " --type=merge -p " + shell.Quote(string(data)))
		if err == nil {
			return name, true, nil
		}
		// K3s's own controllers keep updating a newly registered node, so
		// a conflict only means re-reading it and recomputing the patch.
		if !strings.Contains(err.Error(), "(Conflict)") || attempt == 5 {
			return name, false, fmt.Errorf("updating labels and taints on node %s: %w", name, err)
		}
		if err := pause(ctx, interval); err != nil {
			return name, false, err
		}
		if current, err = get(); err != nil {
			return name, false, err
		}
	}
}

func pause(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type nodeObject struct {
	Metadata struct {
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		// Kept whole, so taints quick8s doesn't manage survive a rewrite
		// with every field intact.
		Taints []map[string]any `json:"taints"`
	} `json:"spec"`
}

// metadataPatch builds a JSON merge patch taking a node from its current
// labels and taints to want. Taints are a list, so a merge patch replaces
// them whole: the patch carries resourceVersion, and the API server rejects
// it if the node changed in between.
func metadataPatch(n nodeObject, want NodeMetadata) (map[string]any, bool) {
	meta := map[string]any{"resourceVersion": n.Metadata.ResourceVersion}
	patch := map[string]any{"metadata": meta}
	changed := false

	labels := map[string]any{}
	for k, v := range want.Labels {
		if cur, ok := n.Metadata.Labels[k]; !ok || cur != v {
			labels[k] = v
		}
	}
	for _, k := range split(n.Metadata.Annotations[managedLabelsAnnotation]) {
		if _, keep := want.Labels[k]; !keep {
			if _, ok := n.Metadata.Labels[k]; ok {
				labels[k] = nil
			}
		}
	}
	if len(labels) > 0 {
		meta["labels"] = labels
		changed = true
	}

	ids := make([]string, len(want.Taints))
	for i, t := range want.Taints {
		ids[i] = t.id()
	}
	slices.Sort(ids)
	managed := append(split(n.Metadata.Annotations[managedTaintsAnnotation]), ids...)
	var taints []map[string]any
	for _, t := range n.Spec.Taints {
		if !slices.Contains(managed, taintOf(t).id()) {
			taints = append(taints, t)
		}
	}
	for _, t := range want.Taints {
		m := map[string]any{"key": t.Key, "effect": t.Effect}
		if t.Value != "" {
			m["value"] = t.Value
		}
		taints = append(taints, m)
	}
	if !sameTaints(n.Spec.Taints, taints) {
		patch["spec"] = map[string]any{"taints": taints}
		changed = true
	}

	annotations := map[string]any{}
	for key, value := range map[string]string{
		managedLabelsAnnotation: strings.Join(slices.Sorted(maps.Keys(want.Labels)), ","),
		managedTaintsAnnotation: strings.Join(ids, ","),
	} {
		cur, ok := n.Metadata.Annotations[key]
		switch {
		case value == "" && ok:
			annotations[key] = nil
		case value != "" && cur != value:
			annotations[key] = value
		}
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
		changed = true
	}
	return patch, changed
}

func taintOf(m map[string]any) Taint {
	s := func(k string) string { v, _ := m[k].(string); return v }
	return Taint{Key: s("key"), Value: s("value"), Effect: s("effect")}
}

func sameTaints(a, b []map[string]any) bool {
	key := func(ts []map[string]any) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			t := taintOf(t)
			out[i] = t.id() + "=" + t.Value
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
