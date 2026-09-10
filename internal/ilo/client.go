// Package ilo speaks to an HPE iLO 4 BMC over SSH.
//
// Two properties of that BMC shape everything here.
//
// First, the fan commands are silent. Every "fan" subcommand, including
// "fan info", returns no output at all: no data, no status line, no error.
// A command that was accepted and a command that was discarded look
// identical. So every write must be confirmed by reading the fan speeds back
// through a different command, and this package refuses to report success on
// the basis of a write alone.
//
// Second, the fan commands only exist on firmware patched with ilo4_unlock.
// An HPE firmware upgrade silently reverts the patch, at which point the
// writes keep appearing to succeed and stop having any effect. The read-back
// is what turns that into a detectable, alertable condition instead of a
// machine that quietly stops being cooled.
package ilo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// prompt is what the iLO CLP shell emits when it is ready for input.
const prompt = "hpiLO->"

// Client is a single long-lived SSH shell session against the BMC.
//
// iLO 4 allows very few concurrent SSH sessions and its handshake is slow, so
// one session is held open and reused rather than reconnecting per command.
type Client struct {
	cfg config.ILO

	mu    sync.Mutex
	conn  *ssh.Client
	sess  *ssh.Session
	stdin io.WriteCloser
	out   *syncBuffer
}

// New returns a client. It does not connect.
func New(cfg config.ILO) *Client { return &Client{cfg: cfg} }

// Connected reports whether a session is currently established.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess != nil
}

// Connect establishes the SSH session and waits for the first prompt.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		return nil
	}

	auth, err := c.authMethod()
	if err != nil {
		return err
	}
	hostKeyCallback, err := c.hostKeyCallback()
	if err != nil {
		return err
	}

	clientCfg := &ssh.ClientConfig{
		User:              c.cfg.User,
		Auth:              []ssh.AuthMethod{auth},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: c.cfg.HostKeyAlgorithms,
		Timeout:           c.cfg.ConnectTimeout.Duration,
	}
	// iLO 4 speaks SHA-1 era key exchange and host keys that current Go
	// disables by default, so they are named explicitly.
	clientCfg.Config.KeyExchanges = c.cfg.KexAlgorithms
	if len(c.cfg.Ciphers) > 0 {
		clientCfg.Config.Ciphers = c.cfg.Ciphers
	}
	if len(c.cfg.MACs) > 0 {
		clientCfg.Config.MACs = c.cfg.MACs
	}

	addr := net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port))
	dialer := &net.Dialer{Timeout: c.cfg.ConnectTimeout.Duration}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, clientCfg)
	if err != nil {
		netConn.Close()
		return fmt.Errorf("ssh handshake with %s: %w", addr, err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)

	sess, err := conn.NewSession()
	if err != nil {
		conn.Close()
		return fmt.Errorf("open session: %w", err)
	}
	// The CLP shell wants a terminal. Without one, iLO's behaviour across
	// firmware builds is inconsistent.
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 9600, ssh.TTY_OP_OSPEED: 9600}
	if err := sess.RequestPty("vt100", 200, 80, modes); err != nil {
		sess.Close()
		conn.Close()
		return fmt.Errorf("request pty: %w", err)
	}
	buf := &syncBuffer{}
	sess.Stdout = buf
	sess.Stderr = buf
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		conn.Close()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		conn.Close()
		return fmt.Errorf("start shell: %w", err)
	}

	c.conn, c.sess, c.stdin, c.out = conn, sess, stdin, buf

	// Drain the login banner up to the first prompt.
	if _, err := c.waitPrompt(ctx, 20*time.Second); err != nil {
		c.closeLocked()
		return fmt.Errorf("waiting for first prompt: %w", err)
	}
	return nil
}

// Close tears the session down.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *Client) closeLocked() {
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.sess != nil {
		_ = c.sess.Close()
		c.sess = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.out = nil
}

// run writes one command and returns everything printed before the next
// prompt.
//
// Every failure here tears the session down, and that is the whole recovery
// mechanism: Connect is a no-op while c.sess is non-nil, so a session left in
// place after a failure is a session the next cycle will happily keep using
// and never rebuild. One BMC reset, or one iLO idle timeout, and the daemon
// writes into a dead pipe for as long as it runs.
//
// Timeouts and cancellations tear down too, not only write errors. A command
// went out and its response did not come back, so anything still in flight
// arrives during some later command's wait and is read as that command's
// answer. A fan speed belonging to the previous request is worse than no fan
// speed at all. Reconnecting costs one handshake on the next cycle; the
// caller sees the error either way.
func (c *Client) run(ctx context.Context, cmd string) (string, error) {
	if c.sess == nil {
		return "", fmt.Errorf("not connected")
	}
	c.out.Reset()
	if _, err := io.WriteString(c.stdin, cmd+"\r\n"); err != nil {
		c.closeLocked()
		return "", fmt.Errorf("write %q: %w", cmd, err)
	}
	// iLO's shell drops commands sent back to back. Upstream testing settled
	// on roughly 350ms between writes.
	select {
	case <-time.After(c.cfg.CommandInterval.Duration):
	case <-ctx.Done():
		c.closeLocked()
		return "", ctx.Err()
	}
	out, err := c.waitPrompt(ctx, 20*time.Second)
	if err != nil {
		c.closeLocked()
	}
	return out, err
}

// waitPrompt polls the output buffer until the CLP prompt reappears.
func (c *Client) waitPrompt(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		s := c.out.String()
		if strings.Contains(s, prompt) {
			return s, nil
		}
		if time.Now().After(deadline) {
			return s, fmt.Errorf("timed out after %s waiting for prompt (got %d bytes)", timeout, len(s))
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return s, ctx.Err()
		}
	}
}

// SetFanMin applies a minimum PWM to one fan.
//
// raw is on iLO's 0..255 scale. There is deliberately no SetFanMax: capping a
// fan can starve the machine of cooling, and upstream attributes a
// fans-to-100-percent thermal shutdown partly to a cap left applied. This
// program only ever raises floors, leaving the BMC's own curve free to run the
// fans faster whenever it wants to.
func (c *Client) SetFanMin(ctx context.Context, index, raw int) error {
	if raw < 0 || raw > 255 {
		return fmt.Errorf("raw pwm %d out of range 0..255", raw)
	}
	if index < 0 {
		return fmt.Errorf("fan index %d out of range", index)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The command prints nothing on success and nothing on failure. Its
	// output is therefore not inspected; correctness is established by the
	// caller reading speeds back.
	_, err := c.run(ctx, fmt.Sprintf("fan p %d min %d", index, raw))
	return err
}

var (
	fanTargetRe = regexp.MustCompile(`/system1/fan(\d+)`)
	desiredRe   = regexp.MustCompile(`DesiredSpeed=(\d+)\s*percent`)
)

// FanSpeeds reads every fan's current speed from the SMASH CLP.
//
// This is the read-back that makes the silent fan commands verifiable, and it
// needs no Redfish client: "show -a /system1/fan1" returns all fan targets in
// one response. The returned map is keyed by zero-based iLO fan index, so
// /system1/fan1 appears as key 0, matching the indexing "fan p" uses.
func (c *Client) FanSpeeds(ctx context.Context) (map[int]float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.run(ctx, "show -a /system1/fan1")
	if err != nil {
		return nil, err
	}
	return parseFanSpeeds(out)
}

func parseFanSpeeds(out string) (map[int]float64, error) {
	speeds := map[int]float64{}
	current := -1
	for _, line := range strings.Split(out, "\n") {
		if m := fanTargetRe.FindStringSubmatch(line); m != nil {
			n, convErr := strconv.Atoi(m[1])
			if convErr == nil && n >= 1 {
				current = n - 1 // CLP labels from 1, "fan p" indexes from 0
			}
			continue
		}
		if current < 0 {
			continue
		}
		if m := desiredRe.FindStringSubmatch(line); m != nil {
			v, convErr := strconv.ParseFloat(m[1], 64)
			if convErr == nil {
				speeds[current] = v
			}
			current = -1
		}
	}
	if len(speeds) == 0 {
		return nil, fmt.Errorf("no fan speeds parsed from CLP output (%d bytes)", len(out))
	}
	return speeds, nil
}

func (c *Client) authMethod() (ssh.AuthMethod, error) {
	if c.cfg.PasswordFile != "" {
		b, err := os.ReadFile(c.cfg.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read password file: %w", err)
		}
		return ssh.Password(strings.TrimRight(string(b), "\r\n")), nil
	}
	b, err := os.ReadFile(c.cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", c.cfg.PrivateKeyPath, err)
	}
	// iLO 4 predates the rsa-sha2 signature algorithms. Go prefers those for
	// RSA keys, and iLO rejects them, so an RSA key is pinned to the legacy
	// ssh-rsa algorithm. Other key types are left alone.
	if signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		if as, ok := signer.(ssh.AlgorithmSigner); ok {
			legacy, wrapErr := ssh.NewSignerWithAlgorithms(as, []string{ssh.KeyAlgoRSA})
			if wrapErr == nil {
				signer = legacy
			}
		}
	}
	return ssh.PublicKeys(signer), nil
}

func (c *Client) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if strings.TrimSpace(c.cfg.HostKey) == "" {
		// Permitted, because pinning a BMC key is awkward on first setup, but
		// never silently: the caller logs this at every startup.
		return ssh.InsecureIgnoreHostKey(), nil
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.cfg.HostKey))
	if err != nil {
		return nil, fmt.Errorf("parse ilo.host_key: %w", err)
	}
	return ssh.FixedHostKey(pub), nil
}

// HostKeyPinned reports whether a host key was configured.
func (c *Client) HostKeyPinned() bool { return strings.TrimSpace(c.cfg.HostKey) != "" }

// syncBuffer is a bytes.Buffer safe for the session's writer goroutine and the
// polling reader to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}
