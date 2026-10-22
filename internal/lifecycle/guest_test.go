package lifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"quick8s/internal/node"
	"quick8s/internal/node/nodetest"
)

type exitStatus int

func (e exitStatus) Error() string   { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitStatus() int { return int(e) }

func TestCloudInitResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		ok, warn bool
	}{
		{"done", nil, true, false},
		{"recoverable errors", fmt.Errorf("wrapped: %w", exitStatus(2)), true, true},
		{"failed", exitStatus(1), false, false},
		{"timed out", exitStatus(124), false, false},
		{"connection lost", errors.New("EOF"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warned := false
			err := cloudInitResult(tc.err, func(string) { warned = true })
			if (err == nil) != tc.ok || warned != tc.warn {
				t.Fatalf("got %v (warned %v)", err, warned)
			}
		})
	}
}

// A rejected key, or no agent, ends the wait at once instead of retrying
// until the provisioning deadline.
func TestDialGuestStopsOnAuthFailure(t *testing.T) {
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, errors.New("key rejected")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				ssh.NewServerConn(conn, config)
			}()
		}
	}()
	target := node.Target{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "quick8s"}

	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodetest.Agent(t, clientKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := dialGuest(ctx, target); !errors.Is(err, node.ErrAuth) {
		t.Fatalf("expected ErrAuth without retrying, got %v", err)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := dialGuest(ctx, target); !errors.Is(err, node.ErrAuth) {
		t.Fatalf("expected ErrAuth without an agent, got %v", err)
	}
}

func TestDialGuestWaitsForPort(t *testing.T) {
	nodetest.Agent(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = dialGuest(ctx, node.Target{Host: "127.0.0.1", Port: port, User: "quick8s"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected to keep waiting until the deadline, got %v", err)
	}
}
