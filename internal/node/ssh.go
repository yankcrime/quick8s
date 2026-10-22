package node

import (
	"bytes"
	"cmp"
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

// ErrAuth marks a Dial failure that retrying won't fix, unlike an
// unreachable or still-booting host: there are no keys to offer, a key
// file can't be unlocked, or the server rejected every key offered.
var ErrAuth = errors.New("SSH authentication failed")

// Dial connects to the given target over SSH, authenticating with the
// running SSH agent's keys first, then the private key at KeyPath, if set.
func Dial(t Target) (*Client, error) {
	// The agent is only needed during authentication. Dial owns its socket,
	// including cleanup when key parsing or the SSH handshake fails. It's
	// optional when a key file is given.
	agentConn, agentErr := dialAgent()
	if agentConn != nil {
		defer agentConn.Close()
	}
	if agentConn == nil && t.KeyPath == "" {
		return nil, fmt.Errorf("%w; or pass --ssh-key (cluster.ssh.key)", agentErr)
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
			return nil, fmt.Errorf("dialing %s: %w: %w\nhint: no offered key was accepted for %s@%s; load the right one into your SSH agent with `ssh-add <key>` (check what's loaded with `ssh-add -l`), or pass --ssh-key",
				addr, ErrAuth, err, t.User, t.Host)
		}
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	return &Client{conn: conn}, nil
}

func dialAgent() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("%w: no SSH agent is running (SSH_AUTH_SOCK is unset); start one and load your key with `ssh-add <key>`", ErrAuth)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w: connecting to the SSH agent at %s: %v", ErrAuth, sock, err)
	}
	return conn, nil
}

// AgentKey returns an authorized_keys line for the one plain key in the
// running SSH agent, for authorizing on a new machine. Certificates are
// skipped, and so are keys the agent also holds a certificate for, as
// Teleport and similar tools load them: those are for certificate
// authentication, not authorized_keys. Zero or several candidates is an
// error naming them, since guessing would lock the user out.
func AgentKey() (string, error) {
	conn, err := dialAgent()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		return "", fmt.Errorf("listing SSH agent keys: %w", err)
	}
	certified := map[string]bool{}
	type plain struct {
		key     ssh.PublicKey
		comment string
	}
	var plains []plain
	for _, k := range keys {
		pub, err := ssh.ParsePublicKey(k.Blob)
		if err != nil {
			continue
		}
		if cert, ok := pub.(*ssh.Certificate); ok {
			certified[string(cert.Key.Marshal())] = true
			continue
		}
		plains = append(plains, plain{pub, k.Comment})
	}
	var candidates []plain
	var names []string
	for _, p := range plains {
		if !certified[string(p.key.Marshal())] {
			candidates = append(candidates, p)
			names = append(names, cmp.Or(p.comment, ssh.FingerprintSHA256(p.key)))
		}
	}
	switch len(candidates) {
	case 0:
		return "", errors.New("the SSH agent holds no plain key to authorize; load one with `ssh-add <key>`, or set cluster.ssh.publicKey")
	case 1:
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(candidates[0].key)))
		if c := candidates[0].comment; c != "" {
			line += " " + c
		}
		return line, nil
	default:
		return "", fmt.Errorf("the SSH agent holds several keys (%s); set cluster.ssh.publicKey to the one to authorize", strings.Join(names, ", "))
	}
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

// authMethods offers the SSH agent's keys, then the private key at keyPath,
// if given. Both go in one publickey method: x/crypto never tries a second
// method of the same name once the first fails, so a separate method would
// never offer the key file.
func authMethods(keyPath string, agentConn net.Conn) ([]ssh.AuthMethod, error) {
	var key ssh.Signer
	if keyPath != "" {
		var err error
		if key, err = loadPrivateKey(keyPath); err != nil {
			return nil, err
		}
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		var signers []ssh.Signer
		if agentConn != nil {
			agentSigners, err := agent.NewClient(agentConn).Signers()
			if err != nil && key == nil {
				return nil, err
			}
			signers = agentSigners
		}
		if key != nil {
			signers = append(signers, key)
		}
		return signers, nil
	})}, nil
}

// unlocked caches decrypted keys by path, so a passphrase is asked for once
// per run, however many nodes or connection attempts use the key.
var unlocked = map[string]ssh.Signer{}

// readPassphrase prompts on stderr; tests replace it.
var readPassphrase = func(path string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", path)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return passphrase, err
}

// loadPrivateKey reads and parses a private key file. An encrypted key in
// OpenSSH's format stores its public half in the clear, so its passphrase
// is only asked for if a server accepts that public key, which never
// happens when the agent already holds the key. Other encrypted keys are
// unlocked straight away.
func loadPrivateKey(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading private key %s: %v", ErrAuth, path, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err == nil {
		return signer, nil
	}

	var passphraseErr *ssh.PassphraseMissingError
	if !errors.As(err, &passphraseErr) {
		return nil, fmt.Errorf("%w: parsing private key %s: %v", ErrAuth, path, err)
	}
	locked := &lockedKey{path: path, pem: key}
	if passphraseErr.PublicKey == nil {
		return locked.unlock()
	}
	locked.pub = passphraseErr.PublicKey
	return locked, nil
}

// lockedKey is an encrypted private key, unlocked on first use.
type lockedKey struct {
	path string
	pem  []byte
	pub  ssh.PublicKey
}

func (k *lockedKey) PublicKey() ssh.PublicKey { return k.pub }

func (k *lockedKey) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	signer, err := k.unlock()
	if err != nil {
		return nil, err
	}
	return signer.Sign(rand, data)
}

func (k *lockedKey) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	signer, err := k.unlock()
	if err != nil {
		return nil, err
	}
	as, ok := signer.(ssh.AlgorithmSigner)
	if !ok {
		return nil, fmt.Errorf("%w: private key %s can't sign with %s", ErrAuth, k.path, algorithm)
	}
	return as.SignWithAlgorithm(rand, data, algorithm)
}

// unlock failures are ErrAuth, so callers that retry don't prompt again
// until their deadline (or forever, with no terminal to prompt on).
func (k *lockedKey) unlock() (ssh.Signer, error) {
	if signer, ok := unlocked[k.path]; ok {
		return signer, nil
	}
	passphrase, err := readPassphrase(k.path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading passphrase for %s: %v", ErrAuth, k.path, err)
	}
	signer, err := ssh.ParsePrivateKeyWithPassphrase(k.pem, passphrase)
	if err != nil {
		return nil, fmt.Errorf("%w: unlocking private key %s: %v", ErrAuth, k.path, err)
	}
	unlocked[k.path] = signer
	return signer, nil
}
