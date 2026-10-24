// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Options is an ssh command line: ssh [-tTv] [-l user] [-p port] [-o k=v]
// [user@]host [command...].
type Options struct {
	User    string
	Host    string
	Port    int
	Command []string
	Verbose int
	TTY     int // 0 auto, 1 forced (-t), -1 disabled (-T)
	Opts    map[string]string
}

func ParseArgs(argv []string) (*Options, error) {
	o := &Options{Opts: map[string]string{}}
	needValue := func(i *int, flag string) (string, error) {
		if *i+1 >= len(argv) {
			return "", fmt.Errorf("option requires an argument -- %s", flag)
		}
		*i++
		return argv[*i], nil
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			i++
			if i < len(argv) && o.Host == "" {
				o.Host = argv[i]
				i++
			}
			o.Command = append(o.Command, argv[i:]...)
			break
		}
		if o.Host != "" {
			o.Command = append(o.Command, argv[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			o.Host = arg
			continue
		}
		for j := 1; j < len(arg); j++ {
			flag := arg[j : j+1]
			rest := arg[j+1:]
			value := func() (string, error) {
				if rest != "" {
					j = len(arg)
					return rest, nil
				}
				j = len(arg)
				return needValue(&i, flag)
			}
			switch flag {
			case "v":
				o.Verbose++
			case "t":
				o.TTY = 1
			case "T":
				o.TTY = -1
			case "q", "4", "6", "A", "a", "x", "X", "C", "N":
			case "p":
				v, err := value()
				if err != nil {
					return nil, err
				}
				port, err := strconv.Atoi(v)
				if err != nil || port < 1 || port > 65535 {
					return nil, fmt.Errorf("Bad port '%s'", v)
				}
				o.Port = port
			case "l":
				v, err := value()
				if err != nil {
					return nil, err
				}
				o.User = v
			case "o":
				v, err := value()
				if err != nil {
					return nil, err
				}
				key, val, ok := splitKeyword(v)
				if !ok {
					return nil, fmt.Errorf("no argument after keyword %q", v)
				}
				if _, set := o.Opts[key]; !set {
					o.Opts[key] = val
				}
			default:
				return nil, fmt.Errorf("unknown option -- %s", flag)
			}
		}
	}
	if o.Host == "" {
		return nil, errors.New("usage: ssh [-tTv] [-l login_name] [-o option] [-p port] destination [command]")
	}
	if at := strings.LastIndex(o.Host, "@"); at >= 0 {
		if o.User == "" {
			o.User = o.Host[:at]
		}
		o.Host = o.Host[at+1:]
	}
	return o, nil
}

// splitKeyword splits "Keyword value", "Keyword=value" or "Keyword = value";
// the keyword is lowercased.
func splitKeyword(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return "", "", false
	}
	key := strings.ToLower(line[:i])
	val := strings.TrimSpace(line[i:])
	val = strings.TrimSpace(strings.TrimPrefix(val, "="))
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		val = val[1 : len(val)-1]
	}
	return key, val, val != ""
}

type configBlock struct {
	patterns []string // nil: applies to every host (before the first Host)
	match    bool     // a Match block, which this parser never applies
	values   [][2]string
}

// ParseConfig reads an OpenSSH client config: Host blocks with *, ? and !
// patterns; Match blocks and Include are ignored.
func ParseConfig(text string) []configBlock {
	blocks := []configBlock{{}}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitKeyword(line)
		if !ok {
			continue
		}
		switch key {
		case "host":
			blocks = append(blocks, configBlock{patterns: strings.Fields(val)})
		case "match":
			blocks = append(blocks, configBlock{match: true})
		default:
			b := &blocks[len(blocks)-1]
			b.values = append(b.values, [2]string{key, val})
		}
	}
	return blocks
}

func hostMatches(patterns []string, host string) bool {
	matched := false
	for _, p := range patterns {
		negate := strings.HasPrefix(p, "!")
		p = strings.ToLower(strings.TrimPrefix(p, "!"))
		h := strings.ToLower(host)
		// ssh patterns know only * and ?; "[host]:port" brackets are literal.
		ok := p == h
		if !ok && !strings.ContainsAny(p, "[]\\") {
			ok, _ = path.Match(p, h)
		}
		if ok && negate {
			return false
		}
		matched = matched || ok
	}
	return matched
}

// lookup returns the first value for key in the blocks that apply to host,
// as OpenSSH does (first obtained value wins).
func lookup(blocks []configBlock, host, key string) string {
	for _, b := range blocks {
		if b.match || (b.patterns != nil && !hostMatches(b.patterns, host)) {
			continue
		}
		for _, kv := range b.values {
			if kv[0] == key {
				return kv[1]
			}
		}
	}
	return ""
}

// Target is a resolved destination.
type Target struct {
	Alias        string // the host as typed (a ComputeContract name)
	HostName     string
	User         string
	Port         int
	ProxyCommand string
	Strict       string // yes | no | accept-new
}

// Resolve applies, in OpenSSH order: command line (-l, user@, -p, -o), then
// the config file. User defaults to root, as the guests' cloud-init does.
func Resolve(o *Options, blocks []configBlock) (Target, error) {
	get := func(key string) string {
		if v, ok := o.Opts[key]; ok {
			return v
		}
		return lookup(blocks, o.Host, key)
	}
	t := Target{Alias: o.Host, HostName: o.Host, User: o.User, Port: o.Port}
	if v := get("hostname"); v != "" {
		t.HostName = strings.ReplaceAll(v, "%h", o.Host)
	}
	if t.User == "" {
		t.User = get("user")
	}
	if t.User == "" {
		t.User = "root"
	}
	if t.Port == 0 {
		if v := get("port"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 65535 {
				return t, fmt.Errorf("Bad port '%s'", v)
			}
			t.Port = p
		}
	}
	if t.Port == 0 {
		t.Port = 22
	}
	switch strings.ToLower(get("stricthostkeychecking")) {
	case "", "yes", "ask":
		t.Strict = "yes"
	case "no", "off":
		t.Strict = "no"
	case "accept-new":
		t.Strict = "accept-new"
	default:
		return t, fmt.Errorf("unsupported option \"StrictHostKeyChecking %s\"", get("stricthostkeychecking"))
	}
	if pc := get("proxycommand"); pc != "" && !strings.EqualFold(pc, "none") {
		t.ProxyCommand = expandTokens(pc, t)
	}
	return t, nil
}

func expandTokens(s string, t Target) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'h':
			b.WriteString(t.HostName)
		case 'n':
			b.WriteString(t.Alias)
		case 'p':
			b.WriteString(strconv.Itoa(t.Port))
		case 'r':
			b.WriteString(t.User)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ProxyTarget checks that the ProxyCommand is the built-in kcp-webrtc proxy
// and returns its host and port.
func ProxyTarget(t Target) (string, int, error) {
	if t.ProxyCommand == "" {
		return "", 0, fmt.Errorf("connect to host %s port %d: direct TCP is not available in the browser; use \"ProxyCommand kcp-webrtc %%h %%p\"", t.HostName, t.Port)
	}
	f := strings.Fields(t.ProxyCommand)
	if f[0] != "kcp-webrtc" {
		return "", 0, fmt.Errorf("ProxyCommand %q is not supported in the browser; only the built-in \"kcp-webrtc %%h %%p\" is", t.ProxyCommand)
	}
	if len(f) != 3 {
		return "", 0, fmt.Errorf("usage: ProxyCommand kcp-webrtc %%h %%p (got %q)", t.ProxyCommand)
	}
	port, err := strconv.Atoi(f[2])
	if err != nil {
		return "", 0, fmt.Errorf("kcp-webrtc: bad port %q", f[2])
	}
	return f[1], port, nil
}

type hostKeyStatus int

const (
	hostKnown hostKeyStatus = iota
	hostUnknown
	hostChanged
)

// knownHostsName is the known_hosts host field: host, or [host]:port.
func knownHostsName(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

// CheckKnownHosts looks key up in known_hosts text. Changed means a line for
// the host holds a different key of the same type; line is that line number.
// Hashed hosts and @-markers are skipped.
func CheckKnownHosts(text, host string, port int, key ssh.PublicKey) (status hostKeyStatus, line int) {
	name := knownHostsName(host, port)
	status = hostUnknown
	for i, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") || strings.HasPrefix(fields[0], "|") {
			continue
		}
		if !hostMatches(strings.Split(fields[0], ","), name) {
			continue
		}
		known, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if err != nil || known.Type() != key.Type() {
			continue
		}
		if string(known.Marshal()) == string(key.Marshal()) {
			return hostKnown, i + 1
		}
		status, line = hostChanged, i+1
	}
	return status, line
}

// knownTypes are the key types known_hosts holds for host, to restrict the
// server's host key algorithms to ones we can check.
func knownTypes(text, host string, port int) []string {
	name := knownHostsName(host, port)
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 3 || !hostMatches(strings.Split(fields[0], ","), name) {
			continue
		}
		switch fields[1] {
		case ssh.KeyAlgoRSA:
			out = append(out, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			out = append(out, fields[1])
		}
	}
	return out
}

func keyLabel(t string) string {
	switch {
	case t == ssh.KeyAlgoED25519:
		return "ED25519"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ECDSA"
	case t == ssh.KeyAlgoRSA:
		return "RSA"
	}
	return strings.ToUpper(t)
}

func changedHostKeyMessage(alias string, key ssh.PublicKey, file string, line int) string {
	label := keyLabel(key.Type())
	return strings.Join([]string{
		"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@",
		"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @",
		"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@",
		"IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!",
		"Someone could be eavesdropping on you right now (man-in-the-middle attack)!",
		"It is also possible that a host key has just been changed.",
		fmt.Sprintf("The fingerprint for the %s key sent by the remote host is", label),
		ssh.FingerprintSHA256(key) + ".",
		"Please contact your system administrator.",
		fmt.Sprintf("Add correct host key in %s to get rid of this message.", file),
		fmt.Sprintf("Offending %s key in %s:%d", label, file, line),
		fmt.Sprintf("Host key for %s has changed and you have requested strict checking.", alias),
		"Host key verification failed.",
		"",
	}, "\n")
}

func unknownHostKeyMessage(alias string, key ssh.PublicKey) string {
	return fmt.Sprintf("No %s host key is known for %s and you have requested strict checking.\nHost key verification failed.\n", keyLabel(key.Type()), alias)
}
