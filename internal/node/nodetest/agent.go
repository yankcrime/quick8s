// Package nodetest provides test fixtures for code that dials nodes.
package nodetest

import (
	"crypto"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh/agent"
)

// Agent serves an SSH agent holding keys for the rest of the test, and
// points SSH_AUTH_SOCK at it. With no keys, the agent is empty.
func Agent(t *testing.T, keys ...crypto.PrivateKey) {
	t.Helper()
	added := make([]agent.AddedKey, len(keys))
	for i, key := range keys {
		added[i] = agent.AddedKey{PrivateKey: key}
	}
	AgentWith(t, added...)
}

// AgentWith is Agent with full entries, such as certificates or comments.
func AgentWith(t *testing.T, keys ...agent.AddedKey) {
	t.Helper()
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
	keyring := agent.NewKeyring()
	for _, key := range keys {
		if err := keyring.Add(key); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", socket)
}
