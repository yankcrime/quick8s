package node

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// Client is an SSH connection to a Target, used to run remote commands
// during bootstrap.
type Client struct {
	target Target
	conn   *ssh.Client
}

// Dial connects to the given target over SSH, authenticating via the
// running SSH agent by default, or an explicit private key if KeyPath is set.
func Dial(t Target) (*Client, error) {
	methods, agentAvailable, err := authMethods(t.KeyPath)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: methods,
		// TODO: verify host keys against a known_hosts file instead of
		// trusting blindly; fine for an MVP against freshly provisioned nodes.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("dialing %s: %w\nhint: no offered key was accepted for %s@%s; run `ssh-add <key>` to load the right key into your agent (check what's loaded with `ssh-add -l`), or pass --ssh-key explicitly%s",
				addr, err, t.User, t.Host, agentHint(agentAvailable))
		}
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	return &Client{target: t, conn: conn}, nil
}

func agentHint(agentAvailable bool) string {
	if agentAvailable {
		return ""
	}
	return " (no running SSH agent was found; is SSH_AUTH_SOCK set?)"
}

// Close closes the underlying SSH connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Run executes cmd on the remote host in its own session and returns
// combined stdout+stderr.
func (c *Client) Run(cmd string) (string, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening session: %w", err)
	}
	defer session.Close()

	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// WriteFile uploads content to path on the remote host as root, creating
// parent directories as needed. Requires the SSH user to be root or have
// passwordless sudo (see Preflight).
func (c *Client) WriteFile(remotePath string, content []byte) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(content)

	var out bytes.Buffer
	session.Stdout = &out
	session.Stderr = &out

	dir := path.Dir(remotePath)
	cmd := fmt.Sprintf("sudo -n mkdir -p %q && sudo -n tee %q >/dev/null", dir, remotePath)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("writing %s: %w\n%s", remotePath, err, out.String())
	}
	return nil
}

// authMethods builds the list of SSH auth methods to offer, preferring the
// running SSH agent (so users don't need to point at a key file at all) and
// adding an explicit private key on top when one is given.
func authMethods(keyPath string) (methods []ssh.AuthMethod, agentAvailable bool, err error) {
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, dialErr := net.Dial("unix", sock); dialErr == nil {
			agentAvailable = true
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if keyPath != "" {
		signer, err := loadPrivateKey(keyPath)
		if err != nil {
			return nil, agentAvailable, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, agentAvailable, fmt.Errorf("no SSH auth methods available: no running SSH agent found and no --ssh-key provided; run `ssh-add <key>` to load a key into your agent")
	}

	return methods, agentAvailable, nil
}

// loadPrivateKey reads and parses a private key file, prompting for a
// passphrase on stderr if the key is encrypted.
func loadPrivateKey(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading private key %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err == nil {
		return signer, nil
	}

	var passphraseErr *ssh.PassphraseMissingError
	if !errors.As(err, &passphraseErr) {
		return nil, fmt.Errorf("parsing private key %s: %w", path, err)
	}

	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", path)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}

	signer, err = ssh.ParsePrivateKeyWithPassphrase(key, passphrase)
	if err != nil {
		return nil, fmt.Errorf("parsing private key %s: %w", path, err)
	}
	return signer, nil
}
