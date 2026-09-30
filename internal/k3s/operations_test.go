package k3s

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"quick8s/internal/shell"
)

type remoteStub struct {
	run     func(string) (string, error)
	runRoot func(string) (string, error)
}

func (r remoteStub) Run(cmd string) (string, error)       { return r.run(cmd) }
func (r remoteStub) RunAsRoot(cmd string) (string, error) { return r.runRoot(cmd) }

func TestInstallPreservesEnvironmentAndArguments(t *testing.T) {
	const literal = "a'b\" $(printf expanded) `printf expanded` $HOME\nnext"
	const server = "https://[2001:db8::1]:6443"
	for _, tt := range []struct {
		name string
		run  func(rootRunner) error
		want []string
	}{
		{"server defaults", func(c rootRunner) error { return Install(c, InstallOpts{}) }, []string{"", "", "", "server"}},
		{"server options", func(c rootRunner) error {
			return Install(c, InstallOpts{Version: literal, ClusterInit: true, TLSSAN: literal, NodeIP: "2001:db8::1"})
		}, []string{literal, "", "", "server", "--cluster-init", "--tls-san", literal, "--node-ip", "2001:db8::1"}},
		{"agent", func(c rootRunner) error {
			return JoinAgent(c, AgentOpts{Version: literal, ServerURL: server, Token: literal, NodeIP: "2001:db8::2"})
		}, []string{literal, server, literal, "agent", "--node-ip", "2001:db8::2"}},
		{"control plane", func(c rootRunner) error {
			return JoinControlPlane(c, ControlPlaneOpts{Version: literal, ServerURL: server, Token: literal, TLSSAN: literal, NodeIP: "2001:db8::3"})
		}, []string{literal, "", literal, "server", "--server", server, "--tls-san", literal, "--node-ip", "2001:db8::3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			c := remoteStub{runRoot: func(script string) (string, error) {
				// Execute the generated shell with a harmless installer returned by
				// a shell function in place of curl. No network or K3s installation.
				installer := `printf '%s\000' "$INSTALL_K3S_VERSION" "$K3S_URL" "$K3S_TOKEN" "$@"`
				cmd := exec.Command("sh", "-c", "curl() { printf '%s' "+shell.Quote(installer)+"; }; "+script)
				cmd.Env = append(os.Environ(), "INSTALL_K3S_VERSION=", "K3S_URL=", "K3S_TOKEN=")
				out, err := cmd.Output()
				got = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
				return string(out), err
			}}
			if err := tt.run(c); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestInstallDownloadFailure(t *testing.T) {
	c := remoteStub{runRoot: func(script string) (string, error) {
		out, err := exec.Command("sh", "-c", `curl() { printf 'echo should-not-run'; return 22; }; `+script).Output()
		if len(out) != 0 {
			t.Fatalf("installer ran after failed download: %q", out)
		}
		return string(out), err
	}}
	err := Install(c, InstallOpts{})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 22 {
		t.Fatalf("expected preserved download error, got %v", err)
	}
}

func TestPreflight(t *testing.T) {
	failure := errors.New("connection lost")
	for _, tt := range []struct {
		name    string
		rootErr error
		out     string
		runErr  error
		wantErr bool
	}{
		{name: "clean node"},
		{name: "installed", out: "/usr/local/bin/k3s\n", wantErr: true},
		{name: "privilege failure", rootErr: failure, wantErr: true},
		{name: "transport failure", runErr: failure, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := remoteStub{
				runRoot: func(string) (string, error) { return "", tt.rootErr },
				run: func(string) (string, error) {
					if tt.rootErr != nil {
						t.Fatal("continued after privilege failure")
					}
					return tt.out, tt.runErr
				},
			}
			err := Preflight(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if (tt.rootErr != nil || tt.runErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("lost underlying error: %v", err)
			}
		})
	}
}

func TestUninstallFailures(t *testing.T) {
	failure := errors.New("connection lost")
	for _, tt := range []struct {
		name       string
		probe      string
		probeErr   error
		installErr error
		wantCalls  int
	}{
		{name: "missing", wantCalls: 1},
		{name: "probe failure", probeErr: failure, wantCalls: 1},
		{name: "uninstall failure", probe: "installed", installErr: failure, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := remoteStub{runRoot: func(string) (string, error) {
				calls++
				if calls == 1 {
					return tt.probe, tt.probeErr
				}
				return "", tt.installErr
			}}
			err := Uninstall(c)
			if err == nil || calls != tt.wantCalls {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
			if (tt.probeErr != nil || tt.installErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("lost underlying error: %v", err)
			}
			if tt.probeErr != nil && strings.Contains(err.Error(), "does not appear") {
				t.Fatalf("transport error reported as missing install: %v", err)
			}
		})
	}
}

func TestKubeconfigAddresses(t *testing.T) {
	for _, tt := range []struct{ host, address string }{
		{"example.test", "example.test:6443"},
		{"192.0.2.1", "192.0.2.1:6443"},
		{"2001:db8::1", "[2001:db8::1]:6443"},
	} {
		t.Run(tt.host, func(t *testing.T) {
			c := remoteStub{runRoot: func(string) (string, error) {
				return "server: https://127.0.0.1:6443\n", nil
			}}
			out, err := Kubeconfig(c, tt.host)
			if err != nil || out != "server: https://"+tt.address+"\n" {
				t.Fatalf("got %q, %v", out, err)
			}
		})
	}
}
