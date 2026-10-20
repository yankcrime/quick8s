package k3s

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingUpdateLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending")
	c := remoteStub{runRoot: func(cmd string) (string, error) {
		// Run the actual marker commands against a disposable local file.
		cmd = strings.ReplaceAll(cmd, pendingUpdatePath, path)
		cmd = strings.ReplaceAll(cmd, "mkdir -p /etc/rancher/k3s", "true")
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		return string(out), err
	}}
	for _, version := range []string{"", "v1.34.1+k3s1"} {
		if pending, err := ReadPendingUpdate(c); err != nil || pending != nil {
			t.Fatalf("before update: %+v, %v", pending, err)
		}
		if err := BeginUpdate(c, version); err != nil {
			t.Fatal(err)
		}
		pending, err := ReadPendingUpdate(c)
		if err != nil || pending == nil || pending.Version != version {
			t.Fatalf("pending update: %+v, %v", pending, err)
		}
		if err := FinishUpdate(c); err != nil {
			t.Fatal(err)
		}
		if pending, err := ReadPendingUpdate(c); err != nil || pending != nil {
			t.Fatalf("after update: %+v, %v", pending, err)
		}
	}
}
