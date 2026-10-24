/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package real boots the full kcp control plane in an environment without
// real sockets. It supplies a net.Pipe-backed listener and the matching
// dialer so the apiserver's own controllers, informers and clients speak
// ordinary HTTPS over an in-process pipe.
package real

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// PipeListener is a net.Listener whose connections are in-process net.Pipe
// pairs. Addr reports a *net.TCPAddr because k8s.io/apiserver's serving
// options require one even when the listener is not a TCP socket.
type PipeListener struct {
	addr *net.TCPAddr

	conns chan net.Conn
	done  chan struct{}

	mu   sync.Mutex
	once sync.Once
}

// NewPipeListener returns a listener that reports addr and accepts pairs
// created by PipeDialer.
func NewPipeListener(addr *net.TCPAddr) *PipeListener {
	return &PipeListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

var _ net.Listener = (*PipeListener)(nil)

func (l *PipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *PipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *PipeListener) Addr() net.Addr { return l.addr }

func (l *PipeListener) deliver(server net.Conn) error {
	select {
	case l.conns <- server:
		return nil
	case <-l.done:
		return net.ErrClosed
	}
}

// PipeDialer creates one net.Pipe pair per dial: the server half goes to the
// listener, the client half is returned to the dialer.
type PipeDialer struct {
	listener *PipeListener

	timeout time.Duration
}

func NewPipeDialer(listener *PipeListener, timeout time.Duration) *PipeDialer {
	return &PipeDialer{listener: listener, timeout: timeout}
}

var _ interface {
	DialContext(context.Context, string, string) (net.Conn, error)
} = (*PipeDialer)(nil)

func (d *PipeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || d.listener == nil {
		return nil, errors.New("real: no pipe listener")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("real: unsupported network %q", network)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- d.listener.deliver(server) }()
	timer := time.NewTimer(d.timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			_ = client.Close()
			_ = server.Close()
			return nil, err
		}
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	case <-timer.C:
		_ = client.Close()
		_ = server.Close()
		return nil, fmt.Errorf("real: dialing %s: timeout", address)
	}
}
