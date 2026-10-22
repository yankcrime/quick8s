package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"quick8s/internal/node/nodetest"
)

// testSSHServer executes commands only on localhost, with an optional fake
// id/sudo PATH. The fixture never requires root or invokes the real sudo.
func testSSHServer(t *testing.T, env []string, rejectAuth bool) Target {
	t.Helper()
	return testSSHServerAccepting(t, env, func(ssh.PublicKey) bool { return !rejectAuth })
}

// testSSHServerAccepting authenticates only the keys accept allows.
func testSSHServerAccepting(t *testing.T, env []string, accept func(ssh.PublicKey) bool) Target {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !accept(key) {
			return nil, errors.New("key rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		stopClose := context.AfterFunc(ctx, func() { conn.Close() })
		defer stopClose()
		server, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		var sessions sync.WaitGroup
		defer sessions.Wait()
		for incoming := range channels {
			if incoming.ChannelType() != "session" {
				incoming.Reject(ssh.UnknownChannelType, "sessions only")
				continue
			}
			channel, requests, err := incoming.Accept()
			if err != nil {
				continue
			}
			sessions.Add(1)
			go func() {
				defer sessions.Done()
				defer channel.Close()
				for req := range requests {
					if req.Type != "exec" {
						req.Reply(false, nil)
						continue
					}
					var payload struct{ Command string }
					if ssh.Unmarshal(req.Payload, &payload) != nil {
						req.Reply(false, nil)
						return
					}
					req.Reply(true, nil)
					runCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					defer stop()
					cmd := exec.CommandContext(runCtx, "sh", "-c", payload.Command)
					cmd.Env = append(os.Environ(), env...)
					cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
					cmd.WaitDelay = time.Second
					status := uint32(0)
					if err := cmd.Run(); err != nil {
						status = 255
						var exit *exec.ExitError
						if errors.As(err, &exit) {
							status = uint32(exit.ExitCode())
						}
					}
					channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					return
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return Target{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test"}
}

func testClient(t *testing.T, env []string) *Client {
	t.Helper()
	target := testSSHServer(t, env, false)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port)), &ssh.ClientConfig{
		User: target.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{conn: conn}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRunSeparatesOutputAndPreservesError(t *testing.T) {
	c := testClient(t, nil)
	out, err := c.Run("printf payload; printf warning >&2")
	if err != nil || out != "payload" {
		t.Fatalf("got %q, %v", out, err)
	}
	out, err = c.Run("printf partial; printf diagnostic >&2; exit 7")
	var exit *ssh.ExitError
	if out != "partial" || !errors.As(err, &exit) || exit.ExitStatus() != 7 || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("got %q, %v; expected stdout plus wrapped exit status and stderr", out, err)
	}
}

func TestRootOperations(t *testing.T) {
	for _, tt := range []struct {
		name, uid, sudo string
		wantErr         bool
	}{
		{"root without sudo", "0", "exit 99", false},
		{"passwordless sudo", "1000", `test "$1" = -n || exit 98; shift; exec "$@"`, false},
		{"sudo denied", "1000", "printf denied >&2; exit 1", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, script := range map[string]string{"id": "printf " + tt.uid, "sudo": tt.sudo} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			c := testClient(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH")})
			out, err := c.RunAsRoot("printf once; printf failed >&2; exit 7")
			var exit *ssh.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("expected wrapped SSH exit error: %v", err)
			}
			if tt.wantErr {
				if out != "" || exit.ExitStatus() != 1 || !strings.Contains(err.Error(), "denied") {
					t.Fatalf("command ran despite denied sudo: %q, %v", out, err)
				}
			} else if out != "once" || exit.ExitStatus() != 7 {
				t.Fatalf("command failed to run exactly once: %q, %v", out, err)
			}

			// Paths and bytes cross both the root wrapper and the SSH stdin stream.
			remotePath := filepath.Join(dir, "new 'directory'", "$(printf expanded) file")
			content := []byte("first\nsecond\x00third\n")
			err = c.WriteFileAsRoot(remotePath, content)
			if tt.wantErr {
				if err == nil {
					t.Fatal("write succeeded despite denied sudo")
				}
				if _, err := os.Stat(remotePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected file after denied write: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(remotePath)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("uploaded %q, error %v", got, err)
			}
		})
	}
}

func TestDialClosesAgent(t *testing.T) {
	for _, mode := range []string{"success", "handshake failure"} {
		t.Run(mode, func(t *testing.T) {
			// Keep the Unix socket path below the macOS sockaddr_un limit.
			dir, err := os.MkdirTemp("", "q8s-agent-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			socket := filepath.Join(dir, "sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			keyring := agent.NewKeyring()
			if err := keyring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
				t.Fatal(err)
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				agent.ServeAgent(keyring, conn)
			}()
			t.Setenv("SSH_AUTH_SOCK", socket)
			target := testSSHServer(t, nil, mode == "handshake failure")
			c, err := Dial(target)
			if c != nil {
				defer c.Close()
			}
			if (err == nil) != (mode == "success") {
				t.Fatalf("unexpected dial result: %v", err)
			}
			if errors.Is(err, ErrAuth) != (mode == "handshake failure") {
				t.Fatalf("only a rejected key should be ErrAuth: %v", err)
			}
			// Observe closure while the successful SSH connection is still open.
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("agent socket was not closed when Dial returned")
			}
			if c != nil {
				out, err := c.Run("printf connected")
				if err != nil || out != "connected" {
					t.Fatalf("SSH session after agent close: %q, %v", out, err)
				}
			}
		})
	}
}

// signalWriter creates a file on its first write, releasing the remote side.
type signalWriter struct {
	path string
	out  bytes.Buffer
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if w.out.Len() == 0 {
		if err := os.WriteFile(w.path, nil, 0600); err != nil {
			return 0, err
		}
	}
	return w.out.Write(p)
}

func TestStreamAsRootDeliversOutputBeforeExit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "id"), []byte("#!/bin/sh\nprintf 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := testClient(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH")})
	// The command only finishes after the local writer has seen its first
	// line, so a buffering implementation deadlocks until the fixture's
	// timeout kills it.
	w := &signalWriter{path: filepath.Join(dir, "released")}
	err := c.StreamAsRoot("printf 'first\\n'; while [ ! -e "+filepath.Join(dir, "released")+" ]; do sleep 0.05; done; printf 'second\\n'; printf diagnostic >&2; exit 3", w)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("expected wrapped exit status and stderr, got %v", err)
	}
	if w.out.String() != "first\nsecond\n" {
		t.Fatalf("streamed %q", w.out.String())
	}
}

// writeKey writes key as an OpenSSH private key file, encrypted when
// passphrase isn't empty.
func writeKey(t *testing.T, key ed25519.PrivateKey, passphrase string) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(key, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func accepting(key ed25519.PrivateKey) func(ssh.PublicKey) bool {
	want, _ := ssh.NewPublicKey(key.Public())
	return func(got ssh.PublicKey) bool { return bytes.Equal(got.Marshal(), want.Marshal()) }
}

// The agent is preferred but optional: a key file alone is enough.
func TestDialWithoutAgent(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	// Nothing listens here: Dial must fail before touching the network.
	_, err := Dial(Target{Host: "192.0.2.1", Port: 22, User: "test"})
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") || !strings.Contains(err.Error(), "--ssh-key") {
		t.Fatalf("expected ErrAuth naming both ways to authenticate, got %v", err)
	}

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	keyPath := writeKey(t, key, "")
	for name, sock := range map[string]string{"no agent": "", "unreachable agent": filepath.Join(t.TempDir(), "gone")} {
		t.Setenv("SSH_AUTH_SOCK", sock)
		target := testSSHServerAccepting(t, nil, accepting(key))
		target.KeyPath = keyPath
		c, err := Dial(target)
		if err != nil {
			t.Fatalf("%s: dial with key file: %v", name, err)
		}
		c.Close()
	}
}

func TestDialUnlocksKeyFileOnlyWhenNeeded(t *testing.T) {
	_, agentKey, _ := ed25519.GenerateKey(rand.Reader)
	_, fileKey, _ := ed25519.GenerateKey(rand.Reader)
	keyPath := writeKey(t, fileKey, "secret")
	prompts, passphrase := 0, "secret"
	original := readPassphrase
	readPassphrase = func(string) ([]byte, error) { prompts++; return []byte(passphrase), nil }
	t.Cleanup(func() {
		readPassphrase = original
		delete(unlocked, keyPath)
	})
	nodetest.Agent(t, agentKey)

	// The agent's key is accepted: the encrypted key file is never unlocked.
	target := testSSHServerAccepting(t, nil, accepting(agentKey))
	target.KeyPath = keyPath
	c, err := Dial(target)
	if err != nil || prompts != 0 {
		t.Fatalf("dial = %v, prompts = %d", err, prompts)
	}
	c.Close()

	// A wrong passphrase fails as ErrAuth, so retrying callers stop.
	passphrase = "wrong"
	target = testSSHServerAccepting(t, nil, accepting(fileKey))
	target.KeyPath = keyPath
	if _, err := Dial(target); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth for a wrong passphrase, got %v", err)
	}

	// Only the file's key is accepted. It's still offered after the agent's
	// keys fail, and unlocked once, however many connections use it.
	passphrase, prompts = "secret", 0
	for range 2 {
		target := testSSHServerAccepting(t, nil, accepting(fileKey))
		target.KeyPath = keyPath
		c, err := Dial(target)
		if err != nil {
			t.Fatalf("dial with key file after agent: %v", err)
		}
		c.Close()
	}
	if prompts != 1 {
		t.Fatalf("prompted %d times, want once", prompts)
	}
}

func TestAgentKey(t *testing.T) {
	newKey := func() (ed25519.PrivateKey, ssh.PublicKey) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := ssh.NewPublicKey(key.Public())
		if err != nil {
			t.Fatal(err)
		}
		return key, pub
	}
	personal, personalPub := newKey()
	other, _ := newKey()
	// Teleport-style: a short-lived key loaded both plain and certified.
	certified, certifiedPub := newKey()
	caKey, _ := newKey()
	ca, err := ssh.NewSignerFromKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: certifiedPub, CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	line := func(pub ssh.PublicKey, comment string) string {
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment
	}

	nodetest.AgentWith(t,
		agent.AddedKey{PrivateKey: personal, Comment: "me@laptop"},
		agent.AddedKey{PrivateKey: certified, Comment: "teleport:key"},
		agent.AddedKey{PrivateKey: certified, Certificate: cert, Comment: "teleport:cert"},
	)
	if got, err := AgentKey(); err != nil || got != line(personalPub, "me@laptop") {
		t.Fatalf("AgentKey() = %q, %v", got, err)
	}

	nodetest.AgentWith(t, agent.AddedKey{PrivateKey: personal, Comment: "me@laptop"}, agent.AddedKey{PrivateKey: other, Comment: "work"})
	if _, err := AgentKey(); err == nil || !strings.Contains(err.Error(), "me@laptop, work") {
		t.Fatalf("expected an error naming both keys, got %v", err)
	}

	nodetest.AgentWith(t, agent.AddedKey{PrivateKey: certified, Certificate: cert})
	if _, err := AgentKey(); err == nil || !strings.Contains(err.Error(), "no plain key") {
		t.Fatalf("expected no candidates, got %v", err)
	}
}
