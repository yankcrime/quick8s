package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
)

func TestSlowStepHeartbeat(t *testing.T) {
	saved := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = saved })

	failure := errors.New("install failed")
	for _, fail := range []bool{false, true} {
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)
		err := slowStep(cmd, "Installing K3s...", func(progress k3s.Progress) error {
			progress("systemd: Starting k3s")
			time.Sleep(70 * time.Millisecond)
			if fail {
				return failure
			}
			return nil
		})
		out := stderr.String()
		if !strings.HasPrefix(out, "Installing K3s...\n  systemd: Starting k3s\n") || !strings.Contains(out, "  still waiting... (") {
			t.Fatalf("missing title, progress, or heartbeat: %q", out)
		}
		if fail {
			if !errors.Is(err, failure) || strings.Contains(out, "done in") {
				t.Fatalf("failed step: %v, %q", err, out)
			}
		} else if err != nil || !strings.HasSuffix(out, "  done in 0s\n") {
			t.Fatalf("successful step: %v, %q", err, out)
		}
	}
}
