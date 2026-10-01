package k3s

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"quick8s/internal/shell"
)

type remoteStub struct {
	run        func(string) (string, error)
	runRoot    func(string) (string, error)
	streamRoot func(string, io.Writer) error
}

func (r remoteStub) Run(cmd string) (string, error)       { return r.run(cmd) }
func (r remoteStub) RunAsRoot(cmd string) (string, error) { return r.runRoot(cmd) }
func (r remoteStub) StreamAsRoot(cmd string, w io.Writer) error {
	return r.streamRoot(cmd, w)
}

func TestInstallPreservesEnvironmentAndArguments(t *testing.T) {
	const literal = "a'b\" $(printf expanded) `printf expanded` $HOME\nnext"
	const server = "https://[2001:db8::1]:6443"
	for _, tt := range []struct {
		name string
		run  func(rootStreamer) error
		want []string
	}{
		{"server defaults", func(c rootStreamer) error { return Install(c, InstallOpts{}, nil) }, []string{"", "", "", "server"}},
		{"server options", func(c rootStreamer) error {
			return Install(c, InstallOpts{Version: literal, ClusterInit: true, TLSSAN: literal, NodeIP: "2001:db8::1"}, nil)
		}, []string{literal, "", "", "server", "--cluster-init", "--tls-san", literal, "--node-ip", "2001:db8::1"}},
		{"agent", func(c rootStreamer) error {
			return JoinAgent(c, AgentOpts{Version: literal, ServerURL: server, Token: literal, NodeIP: "2001:db8::2"}, nil)
		}, []string{literal, server, literal, "agent", "--node-ip", "2001:db8::2"}},
		{"control plane", func(c rootStreamer) error {
			return JoinControlPlane(c, ControlPlaneOpts{Version: literal, ServerURL: server, Token: literal, TLSSAN: literal, NodeIP: "2001:db8::3"}, nil)
		}, []string{literal, "", literal, "server", "--server", server, "--tls-san", literal, "--node-ip", "2001:db8::3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			c := remoteStub{streamRoot: func(script string, _ io.Writer) error {
				// Execute the generated shell with a harmless installer returned by
				// a shell function in place of curl. No network or K3s installation.
				installer := `printf '%s\000' "$INSTALL_K3S_VERSION" "$K3S_URL" "$K3S_TOKEN" "$@"`
				cmd := exec.Command("sh", "-c", "curl() { printf '%s' "+shell.Quote(installer)+"; }; "+script)
				cmd.Env = append(os.Environ(), "INSTALL_K3S_VERSION=", "K3S_URL=", "K3S_TOKEN=")
				out, err := cmd.Output()
				got = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
				return err
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
	c := remoteStub{streamRoot: func(script string, _ io.Writer) error {
		out, err := exec.Command("sh", "-c", `curl() { printf 'echo should-not-run'; return 22; }; `+script).Output()
		if len(out) != 0 {
			t.Fatalf("installer ran after failed download: %q", out)
		}
		return err
	}}
	err := Install(c, InstallOpts{}, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 22 {
		t.Fatalf("expected preserved download error, got %v", err)
	}
}

func TestInstallReportsInstallerOutput(t *testing.T) {
	c := remoteStub{streamRoot: func(_ string, w io.Writer) error {
		// Split writes mid-line, as SSH delivers them, and end unterminated.
		for _, chunk := range []string{"[INFO]  Finding rel", "ease\n\n[INFO]  systemd: Starting k3s\nCreated sym", "link"} {
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
		}
		return nil
	}}
	var got []string
	if err := Install(c, InstallOpts{}, func(line string) { got = append(got, line) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"Finding release", "systemd: Starting k3s", "Created symlink"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestPreflight(t *testing.T) {
	failure := errors.New("connection lost")
	for _, tt := range []struct {
		name       string
		uid        string
		rootErr    error
		out        string
		runErr     error
		wantErr    bool
		wantReport []string
	}{
		{name: "clean node as root", uid: "0\n", wantReport: []string{"root access: ok (connected as root)", "existing K3s installation: none"}},
		{name: "clean node via sudo", uid: "1000\n", wantReport: []string{"root access: ok (passwordless sudo)", "existing K3s installation: none"}},
		{name: "installed", uid: "0\n", out: "/usr/local/bin/k3s\n", wantErr: true, wantReport: []string{"root access: ok (connected as root)"}},
		{name: "privilege failure", rootErr: failure, wantErr: true},
		{name: "transport failure", runErr: failure, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := remoteStub{
				runRoot: func(string) (string, error) { return "", tt.rootErr },
				run: func(cmd string) (string, error) {
					if tt.rootErr != nil {
						t.Fatal("continued after privilege failure")
					}
					if tt.runErr != nil {
						return "", tt.runErr
					}
					if cmd == "id -u" {
						return tt.uid, nil
					}
					return tt.out, nil
				},
			}
			var report []string
			err := Preflight(c, func(line string) { report = append(report, line) })
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if (tt.rootErr != nil || tt.runErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("lost underlying error: %v", err)
			}
			if !reflect.DeepEqual(report, tt.wantReport) {
				t.Fatalf("reported %#v, want %#v", report, tt.wantReport)
			}
		})
	}
}

func TestUninstall(t *testing.T) {
	failure := errors.New("connection lost")
	for _, tt := range []struct {
		name        string
		probe       string
		probeErr    error
		installErr  error
		wantScript  string
		wantErr     bool
		wantMissing bool
	}{
		{name: "server", probe: "server\n", wantScript: uninstallScriptPath},
		{name: "agent", probe: "agent\n", wantScript: agentUninstallScriptPath},
		{name: "missing", wantErr: true, wantMissing: true},
		{name: "probe failure", probeErr: failure, wantErr: true},
		{name: "uninstall failure", probe: "server\n", installErr: failure, wantScript: uninstallScriptPath, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ran []string
			c := remoteStub{
				run: func(string) (string, error) { return tt.probe, tt.probeErr },
				runRoot: func(cmd string) (string, error) {
					ran = append(ran, cmd)
					return "", tt.installErr
				},
			}
			err := Uninstall(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantScript == "" && len(ran) != 0 || tt.wantScript != "" && !reflect.DeepEqual(ran, []string{tt.wantScript}) {
				t.Fatalf("ran %q, want %q", ran, tt.wantScript)
			}
			if (tt.probeErr != nil || tt.installErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("lost underlying error: %v", err)
			}
			if errors.Is(err, ErrNotInstalled) != tt.wantMissing {
				t.Fatalf("transport error and missing install confused: %v", err)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	for _, tt := range []struct {
		out, want string
		wantErr   bool
	}{
		{out: "k3s version v1.30.4+k3s1 (98262b5d)\ngo version go1.22.5\n", want: "v1.30.4+k3s1"},
		{out: "sh: /usr/local/bin/k3s: not found\n", wantErr: true},
		{out: "", wantErr: true},
	} {
		c := remoteStub{run: func(string) (string, error) { return tt.out, nil }}
		got, err := Version(c)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Fatalf("Version(%q) = %q, %v", tt.out, got, err)
		}
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
