package node

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"

	"quick8s/internal/shell"
)

// Client is an SSH connection to a Target, used to run remote commands
// during bootstrap.
type Client struct {
	conn *ssh.Client
}

// Dial connects to the given target over SSH, authenticating via the
// running SSH agent by default, or an explicit private key if KeyPath is set.
func Dial(t Target) (*Client, error) {
	// The agent is only needed during authentication. Dial owns its socket,
	// including cleanup when key parsing or the SSH handshake fails.
	var agentConn net.Conn
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		agentConn, _ = net.Dial("unix", sock)
		if agentConn != nil {
			defer agentConn.Close()
		}
	}
	methods, err := authMethods(t.KeyPath, agentConn)
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
				addr, err, t.User, t.Host, agentHint(agentConn != nil))
		}
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	return &Client{conn: conn}, nil
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

// Run executes cmd on the remote host and returns stdout only. On failure,
// stderr is included in the error, preserving the underlying SSH error.
func (c *Client) Run(cmd string) (string, error) {
	var out bytes.Buffer
	err := c.run(cmd, nil, &out)
	return out.String(), err
}

// RunAsRoot executes cmd directly as root, or through passwordless sudo.
// The privilege mode is selected before execution; a failed command is never
// retried with different privileges.
func (c *Client) RunAsRoot(cmd string) (string, error) {
	var out bytes.Buffer
	err := c.run(rootCommand(cmd), nil, &out)
	return out.String(), err
}

// StreamAsRoot is RunAsRoot for long-running commands: stdout is written to
// w as the remote produces it instead of being returned at the end.
func (c *Client) StreamAsRoot(cmd string, w io.Writer) error {
	return c.run(rootCommand(cmd), nil, w)
}

func rootCommand(cmd string) string {
	quoted := shell.Quote(cmd)
	return `uid=$(id -u) || exit; if [ "$uid" = 0 ]; then exec sh -c ` + quoted +
		`; else exec sudo -n sh -c ` + quoted + `; fi`
}

func (c *Client) run(cmd string, stdin io.Reader, stdout io.Writer) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}
	defer session.Close()

	session.Stdin = stdin
	session.Stdout = stdout
	var stderr bytes.Buffer
	session.Stderr = &stderr
	err = session.Run(cmd)
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return err
}

// WriteFileAsRoot uploads content as root, creating parent directories as
// needed. It uses the same privilege selection as RunAsRoot.
func (c *Client) WriteFileAsRoot(remotePath string, content []byte) error {
	dir := path.Dir(remotePath)
	cmd := "mkdir -p " + shell.Quote(dir) + " && tee " + shell.Quote(remotePath) + " >/dev/null"
	if err := c.run(rootCommand(cmd), bytes.NewReader(content), io.Discard); err != nil {
		return fmt.Errorf("writing %s: %w", remotePath, err)
	}
	return nil
}

// authMethods builds the list of SSH auth methods to offer, preferring the
// running SSH agent (so users don't need to point at a key file at all) and
// adding an explicit private key on top when one is given.
func authMethods(keyPath string, agentConn net.Conn) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if agentConn != nil {
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers))
	}

	if keyPath != "" {
		signer, err := loadPrivateKey(keyPath)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, errors.New("no SSH auth methods available: no running SSH agent found and no --ssh-key provided; run `ssh-add <key>` to load a key into your agent")
	}

	return methods, nil
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
