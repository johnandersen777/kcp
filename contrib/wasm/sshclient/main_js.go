// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build js && wasm

// Command sshclient is a golang.org/x/crypto/ssh client for a browser Web
// Worker (scripts/browser/ssh_worker.js). The transport is a WebRTC
// datachannel owned by the page: bytes arrive through recv() and leave
// through the send callback. The user key never enters Go: signing is a JS
// callback over a non-extractable WebCrypto Ed25519 key. The host key must
// match one of the pinned authorized_keys lines.
//
//	globalThis.sshStart({user, publicKey (32 raw bytes), hostKeys [lines],
//	  cols, rows, send(u8), sign(u8) -> Promise<u8>, output(u8), status(text),
//	  exit(text)}) -> {recv(u8), eof(), input(u8), resize(cols, rows), close()}
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	js.Global().Set("sshStart", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return start(args[0])
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
		ch <- result{err: errors.New(a[0].Call("toString").String())}
		return nil
	})
	p.Call("then", then).Call("catch", catch)
	r := <-ch
	then.Release()
	catch.Release()
	return r.v, r.err
}

// dcConn is a net.Conn over the page's datachannel. recv runs in a JS
// callback, so it only appends and signals; it never blocks.
type dcConn struct {
	send   js.Value
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newConn(send js.Value) *dcConn {
	c := &dcConn{send: send}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *dcConn) recv(b []byte) {
	c.mu.Lock()
	c.buf = append(c.buf, b...)
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *dcConn) Read(p []byte) (int, error) {
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

func (c *dcConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	c.send.Invoke(toJS(p))
	return len(p), nil
}

func (c *dcConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cond.Broadcast()
	return nil
}

type dcAddr struct{}

func (dcAddr) Network() string { return "webrtc" }
func (dcAddr) String() string  { return "datachannel" }

func (c *dcConn) LocalAddr() net.Addr              { return dcAddr{} }
func (c *dcConn) RemoteAddr() net.Addr             { return dcAddr{} }
func (c *dcConn) SetDeadline(time.Time) error      { return nil }
func (c *dcConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dcConn) SetWriteDeadline(time.Time) error { return nil }

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

// pinned parses authorized_keys lines into a host key callback and the
// algorithms to negotiate, so the server presents a key we can check.
func pinned(lines js.Value) (ssh.HostKeyCallback, []string, error) {
	var keys []ssh.PublicKey
	var algos []string
	for i := 0; i < lines.Length(); i++ {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(lines.Index(i).String()))
		if err != nil {
			return nil, nil, fmt.Errorf("host key %d: %w", i, err)
		}
		keys = append(keys, key)
		if key.Type() == ssh.KeyAlgoRSA {
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		} else {
			algos = append(algos, key.Type())
		}
	}
	if len(keys) == 0 {
		return nil, nil, errors.New("no pinned host keys")
	}
	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		for _, key := range keys {
			if bytes.Equal(key.Marshal(), presented.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("host key %s is not pinned", ssh.FingerprintSHA256(presented))
	}, algos, nil
}

type jsWriter struct{ fn js.Value }

func (w jsWriter) Write(p []byte) (int, error) {
	w.fn.Invoke(toJS(p))
	return len(p), nil
}

func start(cfg js.Value) any {
	conn := newConn(cfg.Get("send"))
	stdin := newConn(js.Undefined())
	var (
		mu      sync.Mutex
		session *ssh.Session
		client  *ssh.Client
	)
	go func() {
		err := run(cfg, conn, stdin, func(c *ssh.Client, s *ssh.Session) {
			mu.Lock()
			client, session = c, s
			mu.Unlock()
		})
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		conn.Close()
		stdin.Close()
		cfg.Get("exit").Invoke(msg)
	}()
	handle := js.Global().Get("Object").New()
	handle.Set("recv", js.FuncOf(func(_ js.Value, a []js.Value) any { conn.recv(bytesOf(a[0])); return nil }))
	handle.Set("eof", js.FuncOf(func(js.Value, []js.Value) any { conn.Close(); return nil }))
	// stdin is a dcConn too: input only appends, so keystrokes keep their order.
	handle.Set("input", js.FuncOf(func(_ js.Value, a []js.Value) any { stdin.recv(bytesOf(a[0])); return nil }))
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
	handle.Set("close", js.FuncOf(func(js.Value, []js.Value) any {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			go c.Close()
		}
		conn.Close()
		return nil
	}))
	return handle
}

func run(cfg js.Value, conn, stdin *dcConn, ready func(*ssh.Client, *ssh.Session)) error {
	status := func(s string) { cfg.Get("status").Invoke(s) }
	hostKey, algos, err := pinned(cfg.Get("hostKeys"))
	if err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(ed25519.PublicKey(bytesOf(cfg.Get("publicKey"))))
	if err != nil {
		return err
	}
	signer := webCryptoSigner{pub: pub, sign: cfg.Get("sign")}
	status("ssh handshake")
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "guest", &ssh.ClientConfig{
		User:              cfg.Get("user").String(),
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: algos,
		Timeout:           30 * time.Second,
	})
	if err != nil {
		return err
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	out := jsWriter{cfg.Get("output")}
	session.Stdin, session.Stdout, session.Stderr = stdin, out, out
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty("xterm-256color", cfg.Get("rows").Int(), cfg.Get("cols").Int(), modes); err != nil {
		return err
	}
	if err := session.Shell(); err != nil {
		return err
	}
	ready(client, session)
	status("shell open")
	return session.Wait()
}
