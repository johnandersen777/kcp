// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build js && wasm

// Command sshclient is the page's `ssh` command: OpenSSH-style argv and
// ~/.ssh/config, known_hosts checking and golang.org/x/crypto/ssh, run in a
// browser Web Worker (scripts/browser/ssh_worker.js). There is no TCP: the
// only ProxyCommand is the built-in `kcp-webrtc %h %p`, whose bytes ride a
// WebRTC datachannel the page opens. The user key never enters Go: signing is
// a JS callback over a non-extractable WebCrypto Ed25519 key.
//
//	globalThis.sshRun({argv, config, interactive, stdin, cols, rows,
//	  resolve(host, port) -> Promise<{publicKey, knownHosts}>,
//	  dial(host, port, recv(u8), eof(), verbose) -> Promise<send(u8)>,
//	  addKnownHost(line), sign(u8) -> Promise<u8>, stdout(u8), stderr(u8),
//	  raw(bool), exit(code)}) -> {input(u8), stdinEOF(), resize(cols, rows)}
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	configPath     = "~/.ssh/config"
	knownHostsPath = "~/.ssh/known_hosts"
)

func main() {
	js.Global().Set("sshRun", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return run(args[0])
	}))
	select {}
}

func bytesOf(v js.Value) []byte {
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

func toJS(b []byte) js.Value {
	u8 := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(u8, b)
	return u8
}

// await blocks the calling goroutine (never a JS callback) on a promise.
func await(p js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	ch := make(chan result, 1)
	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- result{v: a[0]}
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, a []js.Value) any {
		msg := a[0].Get("message")
		if msg.Type() != js.TypeString {
			msg = a[0].Call("toString")
		}
		ch <- result{err: errors.New(msg.String())}
		return nil
	})
	p.Call("then", then).Call("catch", catch)
	r := <-ch
	then.Release()
	catch.Release()
	return r.v, r.err
}

// pipe is a net.Conn over bytes JS pushes in (the datachannel, or the
// terminal's stdin). recv runs in a JS callback, so it only appends and
// signals; it never blocks, and bytes keep their order.
type pipe struct {
	send   js.Value
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newPipe() *pipe {
	c := &pipe{send: js.Undefined()}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *pipe) recv(b []byte) {
	c.mu.Lock()
	c.buf = append(c.buf, b...)
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *pipe) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *pipe) Write(p []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	c.send.Invoke(toJS(p))
	return len(p), nil
}

func (c *pipe) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cond.Broadcast()
	return nil
}

type dcAddr struct{}

func (dcAddr) Network() string { return "webrtc" }
func (dcAddr) String() string  { return "datachannel" }

func (c *pipe) LocalAddr() net.Addr              { return dcAddr{} }
func (c *pipe) RemoteAddr() net.Addr             { return dcAddr{} }
func (c *pipe) SetDeadline(time.Time) error      { return nil }
func (c *pipe) SetReadDeadline(time.Time) error  { return nil }
func (c *pipe) SetWriteDeadline(time.Time) error { return nil }

// webCryptoSigner signs with the page's non-extractable Ed25519 CryptoKey.
type webCryptoSigner struct {
	pub  ssh.PublicKey
	sign js.Value
}

func (s webCryptoSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s webCryptoSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	v, err := await(s.sign.Invoke(toJS(data)))
	if err != nil {
		return nil, fmt.Errorf("webcrypto sign: %w", err)
	}
	return &ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: bytesOf(js.Global().Get("Uint8Array").New(v))}, nil
}

type jsWriter struct{ fn js.Value }

func (w jsWriter) Write(p []byte) (int, error) {
	w.fn.Invoke(toJS(p))
	return len(p), nil
}

var errHostKey = errors.New("host key verification failed")

func run(cfg js.Value) any {
	stdin := newPipe()
	var (
		mu      sync.Mutex
		session *ssh.Session
	)
	go func() {
		code := sshMain(cfg, stdin, func(s *ssh.Session) {
			mu.Lock()
			session = s
			mu.Unlock()
		})
		cfg.Get("exit").Invoke(code)
	}()
	handle := js.Global().Get("Object").New()
	handle.Set("input", js.FuncOf(func(_ js.Value, a []js.Value) any { stdin.recv(bytesOf(a[0])); return nil }))
	handle.Set("stdinEOF", js.FuncOf(func(js.Value, []js.Value) any { stdin.Close(); return nil }))
	handle.Set("resize", js.FuncOf(func(_ js.Value, a []js.Value) any {
		cols, rows := a[0].Int(), a[1].Int()
		mu.Lock()
		s := session
		mu.Unlock()
		if s != nil {
			go s.WindowChange(rows, cols)
		}
		return nil
	}))
	return handle
}

func strings2(v js.Value) []string {
	out := make([]string, v.Length())
	for i := range out {
		out[i] = v.Index(i).String()
	}
	return out
}

// sshMain is ssh(1): it returns the remote exit status, or 255 for its own
// errors, as OpenSSH does.
func sshMain(cfg js.Value, stdin *pipe, ready func(*ssh.Session)) int {
	stderr := jsWriter{cfg.Get("stderr")}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "ssh: "+format+"\n", a...)
		return 255
	}
	o, err := ParseArgs(strings2(cfg.Get("argv")))
	if err != nil {
		return fail("%v", err)
	}
	debug := func(format string, a ...any) {
		if o.Verbose > 0 {
			fmt.Fprintf(stderr, "debug1: "+format+"\n", a...)
		}
	}
	debug("Reading configuration data %s", configPath)
	t, err := Resolve(o, ParseConfig(cfg.Get("config").String()))
	if err != nil {
		return fail("%v", err)
	}
	host, port, err := ProxyTarget(t)
	if err != nil {
		return fail("%v", err)
	}
	debug("Executing proxy command: %s", t.ProxyCommand)
	res, err := await(cfg.Get("resolve").Invoke(host, port))
	if err != nil {
		return fail("Could not resolve hostname %s: %v", host, err)
	}
	knownHosts := res.Get("knownHosts").String()
	pub, err := ssh.NewPublicKey(ed25519.PublicKey(bytesOf(res.Get("publicKey"))))
	if err != nil {
		return fail("identity: %v", err)
	}

	conn := newPipe()
	recv := js.FuncOf(func(_ js.Value, a []js.Value) any { conn.recv(bytesOf(a[0])); return nil })
	eof := js.FuncOf(func(js.Value, []js.Value) any { conn.Close(); return nil })
	defer recv.Release()
	defer eof.Release()
	send, err := await(cfg.Get("dial").Invoke(host, port, recv, eof, o.Verbose > 0))
	if err != nil {
		return fail("kcp-webrtc: connect to host %s port %d: %v", host, port, err)
	}
	conn.send = send
	defer conn.Close()

	hostKey := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		debug("Server host key: %s %s", key.Type(), ssh.FingerprintSHA256(key))
		status, line := CheckKnownHosts(knownHosts, t.HostName, t.Port, key)
		switch {
		case status == hostKnown:
			debug("Host '%s' is known and matches the %s host key.", t.HostName, keyLabel(key.Type()))
			debug("Found key in %s:%d", knownHostsPath, line)
			return nil
		case status == hostChanged:
			io.WriteString(stderr, changedHostKeyMessage(t.Alias, key, knownHostsPath, line))
			return errHostKey
		case t.Strict == "yes":
			io.WriteString(stderr, unknownHostKeyMessage(t.Alias, key))
			return errHostKey
		case t.Strict == "accept-new":
			entry := knownHostsName(t.HostName, t.Port) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			cfg.Get("addKnownHost").Invoke(entry)
			fmt.Fprintf(stderr, "Warning: Permanently added '%s' (%s) to the list of known hosts.\n", t.HostName, keyLabel(key.Type()))
			return nil
		default:
			fmt.Fprintf(stderr, "Warning: %s host key for %s is not verified (StrictHostKeyChecking=no).\n", keyLabel(key.Type()), t.HostName)
			return nil
		}
	}
	debug("Will attempt key: %s %s (WebCrypto, non-extractable)", keyLabel(pub.Type()), ssh.FingerprintSHA256(pub))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, knownHostsName(t.HostName, t.Port), &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(webCryptoSigner{pub: pub, sign: cfg.Get("sign")})},
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: knownTypes(knownHosts, t.HostName, t.Port),
		Timeout:           30 * time.Second,
	})
	switch {
	case errors.Is(err, errHostKey):
		return 255
	case err != nil && strings.Contains(err.Error(), "unable to authenticate"):
		fmt.Fprintf(stderr, "%s@%s: Permission denied (publickey).\n", t.User, t.Alias)
		return 255
	case err != nil:
		return fail("%v", err)
	}
	debug("Authentication succeeded (publickey).")
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fail("%v", err)
	}
	session.Stdout = jsWriter{cfg.Get("stdout")}
	session.Stderr = stderr

	interactive := cfg.Get("interactive").Truthy()
	pty := o.TTY == 1 || (o.TTY == 0 && len(o.Command) == 0 && interactive)
	if len(o.Command) == 0 && !interactive && o.TTY == 0 {
		io.WriteString(stderr, "Pseudo-terminal will not be allocated because stdin is not a terminal.\n")
	}
	if interactive {
		session.Stdin = stdin
	} else if in := cfg.Get("stdin"); in.Truthy() {
		session.Stdin = bytes.NewReader(bytesOf(in))
	} else {
		session.Stdin = bytes.NewReader(nil)
	}
	if pty {
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
		if err := session.RequestPty("xterm-256color", cfg.Get("rows").Int(), cfg.Get("cols").Int(), modes); err != nil {
			return fail("PTY allocation request failed: %v", err)
		}
		cfg.Get("raw").Invoke(true)
	}
	ready(session)
	if len(o.Command) == 0 {
		err = session.Shell()
	} else {
		err = session.Start(strings.Join(o.Command, " "))
	}
	if err != nil {
		return fail("%v", err)
	}
	err = session.Wait()
	var exit *ssh.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitStatus()
	default:
		return 255
	}
}
