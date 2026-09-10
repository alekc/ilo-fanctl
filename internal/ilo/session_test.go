package ilo

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// fakeBMC is an in-process SSH server that answers the way iLO's CLP shell
// does: a prompt when the shell starts, and a prompt after every line it
// reads. It exists because the behaviour under test is not observable from
// outside this package. A Client holding a dead session looks exactly like
// one holding a live session, right up to the point where Connect declines
// to rebuild it and every later cycle writes into a closed pipe.
//
// It can also stop answering (mute) or hang up entirely, which are the two
// ways a real BMC leaves a session behind: an idle timeout on the shell, and
// a reset or a lost network.
type fakeBMC struct {
	ln   net.Listener
	host ssh.Signer

	mu       sync.Mutex
	muted    bool
	conns    []net.Conn
	sessions int
}

func newFakeBMC(t *testing.T) *fakeBMC {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sign with host key: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &fakeBMC{ln: ln, host: signer}
	t.Cleanup(func() {
		_ = ln.Close()
		b.hangUp()
	})
	go b.serve()
	return b
}

func (b *fakeBMC) serve() {
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(b.host)
	for {
		nc, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, nc)
		b.mu.Unlock()
		go b.handle(nc, cfg)
	}
}

func (b *fakeBMC) handle(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.sessions++
		b.mu.Unlock()
		go func() {
			for r := range chReqs {
				switch r.Type {
				case "pty-req":
					_ = r.Reply(true, nil)
				case "shell":
					_ = r.Reply(true, nil)
					go b.shell(ch)
				default:
					if r.WantReply {
						_ = r.Reply(false, nil)
					}
				}
			}
		}()
	}
}

func (b *fakeBMC) shell(ch ssh.Channel) {
	defer ch.Close()
	_, _ = io.WriteString(ch, "\r\n"+prompt+" ")
	sc := bufio.NewScanner(ch)
	for sc.Scan() {
		b.mu.Lock()
		muted := b.muted
		b.mu.Unlock()
		// A muted shell still drains what is sent to it. That is the point:
		// the write succeeds, so only the missing prompt says anything is
		// wrong, and only the caller's own deadline ends the wait.
		if muted {
			continue
		}
		_, _ = io.WriteString(ch, sc.Text()+"\r\n"+prompt+" ")
	}
}

func (b *fakeBMC) mute() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.muted = true
}

func (b *fakeBMC) unmute() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.muted = false
}

// hangUp drops every accepted connection and leaves the listener up, which is
// what a BMC reset looks like from the client's side.
func (b *fakeBMC) hangUp() {
	b.mu.Lock()
	conns := b.conns
	b.conns = nil
	b.mu.Unlock()
	for _, nc := range conns {
		_ = nc.Close()
	}
}

func (b *fakeBMC) sessionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions
}

func (b *fakeBMC) clientConfig(t *testing.T) config.ILO {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pw, []byte("not-a-real-password\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	host, port, err := net.SplitHostPort(b.ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	return config.ILO{
		Host:            host,
		Port:            n,
		User:            "test",
		PasswordFile:    pw,
		ConnectTimeout:  config.Duration{Duration: 5 * time.Second},
		CommandInterval: config.Duration{Duration: 5 * time.Millisecond},
	}
}

// failOnce runs one command under a short deadline. SetFanMin is the command
// used throughout because it ignores the response, so the fake shell does not
// have to produce parseable CLP output for these tests to mean anything.
func failOnce(ctx context.Context, c *Client, d time.Duration) error {
	short, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return c.SetFanMin(short, 0, 90)
}

func TestAShellThatStopsAnsweringIsRebuiltRatherThanKept(t *testing.T) {
	bmc := newFakeBMC(t)
	c := New(bmc.clientConfig(t))
	t.Cleanup(c.Close)

	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect to the fake BMC: %v", err)
	}
	if err := c.SetFanMin(ctx, 0, 90); err != nil {
		t.Fatalf("first command on a fresh session: %v", err)
	}

	bmc.mute()
	if err := failOnce(ctx, c, 300*time.Millisecond); err == nil {
		t.Fatal("a command against a muted shell reported success")
	}

	if c.Connected() {
		t.Error("the timed-out session is still held, so Connect is a no-op and nothing ever rebuilds it")
	}

	bmc.unmute()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("reconnect after the shell went quiet: %v", err)
	}
	if err := c.SetFanMin(ctx, 0, 90); err != nil {
		t.Fatalf("first command on the rebuilt session: %v", err)
	}
	if got := bmc.sessionCount(); got != 2 {
		t.Errorf("BMC opened %d shell sessions, want 2 (the original and the rebuild)", got)
	}
}

func TestABMCThatHangsUpIsRebuiltRatherThanKept(t *testing.T) {
	bmc := newFakeBMC(t)
	c := New(bmc.clientConfig(t))
	t.Cleanup(c.Close)

	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect to the fake BMC: %v", err)
	}
	if err := c.SetFanMin(ctx, 0, 90); err != nil {
		t.Fatalf("first command on a fresh session: %v", err)
	}

	bmc.hangUp()

	// The client learns the connection is gone when its own read loop reaches
	// the EOF, which is a goroutine hop away rather than immediate, so a
	// single write can still land in a buffer and look fine. Retry until one
	// fails instead of sleeping on a guess about how long that takes.
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err = failOnce(ctx, c, time.Second); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("commands kept succeeding against a BMC that had hung up")
	}

	if c.Connected() {
		t.Error("the dead session is still held, so Connect is a no-op and nothing ever rebuilds it")
	}

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("reconnect after the BMC hung up: %v", err)
	}
	if err := c.SetFanMin(ctx, 0, 90); err != nil {
		t.Fatalf("first command on the rebuilt session: %v", err)
	}
	if got := bmc.sessionCount(); got != 2 {
		t.Errorf("BMC opened %d shell sessions, want 2 (the original and the rebuild)", got)
	}
}
