// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testConfig = `
# seeded by the page
Host vmshell-*
  User root
  ProxyCommand kcp-webrtc %h %p

Host other !vmshell-x
  User nobody
  Port=2222

Host *
  StrictHostKeyChecking yes
  User fallback
`

func TestParseArgs(t *testing.T) {
	o, err := ParseArgs([]string{"-vt", "-p", "2200", "-o", "StrictHostKeyChecking=no", "admin@vmshell-a", "uname", "-a"})
	if err != nil {
		t.Fatal(err)
	}
	if o.User != "admin" || o.Host != "vmshell-a" || o.Port != 2200 || o.Verbose != 1 || o.TTY != 1 ||
		o.Opts["stricthostkeychecking"] != "no" || strings.Join(o.Command, " ") != "uname -a" {
		t.Fatalf("%+v", o)
	}
	o, err = ParseArgs([]string{"-l", "bob", "-T", "-p2222", "h", "-v"})
	if err != nil || o.User != "bob" || o.TTY != -1 || o.Port != 2222 || strings.Join(o.Command, " ") != "-v" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{}, {"-Z", "h"}, {"-p"}, {"-p", "x", "h"}} {
		if _, err := ParseArgs(bad); err == nil {
			t.Fatalf("%v: want an error", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	blocks := ParseConfig(testConfig)
	o, _ := ParseArgs([]string{"vmshell-abc"})
	tg, err := Resolve(o, blocks)
	if err != nil || tg.User != "root" || tg.Port != 22 || tg.ProxyCommand != "kcp-webrtc vmshell-abc 22" || tg.Strict != "yes" {
		t.Fatalf("%+v %v", tg, err)
	}
	if h, p, err := ProxyTarget(tg); err != nil || h != "vmshell-abc" || p != 22 {
		t.Fatalf("%s %d %v", h, p, err)
	}
	o, _ = ParseArgs([]string{"-o", "user=me", "-o", "StrictHostKeyChecking accept-new", "other"})
	tg, _ = Resolve(o, blocks)
	if tg.User != "me" || tg.Port != 2222 || tg.Strict != "accept-new" || tg.ProxyCommand != "" {
		t.Fatalf("%+v", tg)
	}
	if _, _, err := ProxyTarget(tg); err == nil || !strings.Contains(err.Error(), "not available in the browser") {
		t.Fatalf("direct TCP: %v", err)
	}
	o, _ = ParseArgs([]string{"-o", "ProxyCommand=nc %h %p", "x"})
	tg, _ = Resolve(o, blocks)
	if _, _, err := ProxyTarget(tg); err == nil || !strings.Contains(err.Error(), "not supported in the browser") {
		t.Fatalf("nc: %v", err)
	}
	o, _ = ParseArgs([]string{"plain"})
	if tg, _ = Resolve(o, blocks); tg.User != "fallback" {
		t.Fatalf("%+v", tg)
	}
}

func TestKnownHosts(t *testing.T) {
	pub := func(seed byte) ssh.PublicKey {
		k, _ := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make32(seed)).Public())
		return k
	}
	line := func(host string, k ssh.PublicKey) string {
		return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	}
	a, b := pub(1), pub(2)
	text := strings.Join([]string{"# comment", line("vmshell-a", a), line("[vmshell-p]:2200,alias", b)}, "\n")
	if s, n := CheckKnownHosts(text, "vmshell-a", 22, a); s != hostKnown || n != 2 {
		t.Fatalf("known: %v %d", s, n)
	}
	if s, n := CheckKnownHosts(text, "vmshell-a", 22, b); s != hostChanged || n != 2 {
		t.Fatalf("changed: %v %d", s, n)
	}
	if s, _ := CheckKnownHosts(text, "vmshell-p", 2200, b); s != hostKnown {
		t.Fatalf("port form: %v", s)
	}
	if s, _ := CheckKnownHosts(text, "vmshell-z", 22, a); s != hostUnknown {
		t.Fatalf("unknown: %v", s)
	}
	if !strings.Contains(changedHostKeyMessage("vmshell-a", b, "~/.ssh/known_hosts", 2), "REMOTE HOST IDENTIFICATION HAS CHANGED") {
		t.Fatal("changed wording")
	}
	if got := knownTypes(text, "vmshell-a", 22); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("types: %v", got)
	}
}

func make32(b byte) []byte {
	s := make([]byte, 32)
	s[0] = b
	return s
}
