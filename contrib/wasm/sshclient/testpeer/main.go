// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Command testpeer stands in for the plan 0034 guest agent so the page's
// WebRTC SSH path can be tested without a bidder: an age X25519 identity, one
// DTLS certificate, a pion answerer whose datachannels bridge to an
// in-process x/crypto/ssh server with a pty shell. Signaling is plain HTTP on
// loopback with the same sealed bodies the ContractEvents carry:
//
//	GET  /guest      -> {key, fingerprint, hostKeys}   (guest publicKey, onNetwork, vm.hostKeys)
//	POST /publicKey  {key, sshAuthorizedKey}            (requester publicKey, spec.sshAuthorizedKeys)
//	POST /offer      {ciphertext} -> {ciphertext}       (signalWebRTC offer -> answer)
//
// Sealed bodies are JSON: offers {sdp, replyTo}, answers {sdp} sealed to replyTo.
//
// It logs only public material: never the identity or a plaintext SDP.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/creack/pty"
	"github.com/pion/webrtc/v4"
	"golang.org/x/crypto/ssh"
)

type peer struct {
	identity *age.X25519Identity
	cert     webrtc.Certificate
	hostKey  ssh.Signer
	sshAddr  string
	stun     string

	mu         sync.Mutex
	authorized ssh.PublicKey
}

func main() {
	listen := flag.String("listen", "127.0.0.1:7795", "signaling HTTP address")
	stun := flag.String("stun", "stun:stun.cloudflare.com:3478", "STUN server (empty: host candidates only)")
	flag.Parse()
	if host, _, err := net.SplitHostPort(*listen); err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalf("-listen must be a loopback address, got %q", *listen)
	}

	p := &peer{stun: *stun}
	var err error
	if p.identity, err = age.GenerateX25519Identity(); err != nil {
		log.Fatal(err)
	}
	certs, err := webrtc.GenerateCertificate(mustECDSA())
	if err != nil {
		log.Fatal(err)
	}
	p.cert = *certs
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	if p.hostKey, err = ssh.NewSignerFromKey(hostPriv); err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	p.sshAddr = ln.Addr().String()
	go p.serveSSH(ln)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /guest", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"key":         p.identity.Recipient().String(),
			"fingerprint": p.fingerprint(),
			"hostKeys":    []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.hostKey.PublicKey())))},
		})
	})
	mux.HandleFunc("POST /publicKey", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Key, SSHAuthorizedKey string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// The key is the generic handshake; answers go to the offer's replyTo.
		if _, err := age.ParseX25519Recipient(in.Key); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		authorized, _, _, _, err := ssh.ParseAuthorizedKey([]byte(in.SSHAuthorizedKey))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.mu.Lock()
		p.authorized = authorized
		p.mu.Unlock()
		log.Printf("requester publicKey %s, ssh key %s", in.Key, ssh.FingerprintSHA256(authorized))
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("POST /offer", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Ciphertext string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		out, err := p.answer(in.Ciphertext)
		if err != nil {
			log.Printf("offer: %v", err)
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]any{"ciphertext": out})
	})
	cors := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "content-type")
		if r.Method == http.MethodOptions {
			return
		}
		mux.ServeHTTP(w, r)
	})
	log.Printf("testpeer signaling http://%s, ssh %s, fingerprint %s", *listen, p.sshAddr, p.fingerprint())
	log.Fatal(http.ListenAndServe(*listen, cors))
}

func mustECDSA() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	return k
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fingerprint is the onNetwork kind webrtc address form, "sha-256 AB:CD:...".
func (p *peer) fingerprint() string {
	fps, err := p.cert.GetFingerprints()
	if err != nil || len(fps) == 0 {
		log.Fatalf("certificate fingerprint: %v", err)
	}
	return fps[0].Algorithm + " " + strings.ToUpper(fps[0].Value)
}

// answer opens a sealed {sdp, replyTo} offer and seals {sdp} back to replyTo,
// the plan 0034 signalWebRTC body.
func (p *peer) answer(ciphertext string) (string, error) {
	r, err := age.Decrypt(armor.NewReader(strings.NewReader(ciphertext)), p.identity)
	if err != nil {
		return "", fmt.Errorf("open offer: %w", err)
	}
	var offer struct {
		SDP     string `json:"sdp"`
		ReplyTo string `json:"replyTo"`
	}
	if err := json.NewDecoder(r).Decode(&offer); err != nil {
		return "", fmt.Errorf("offer body: %w", err)
	}
	requester, err := age.ParseX25519Recipient(offer.ReplyTo)
	if err != nil {
		return "", fmt.Errorf("offer replyTo: %w", err)
	}
	cfg := webrtc.Configuration{Certificates: []webrtc.Certificate{p.cert}}
	if p.stun != "" {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: []string{p.stun}}}
	}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return "", err
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("peer connection %s", s)
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			_ = pc.Close()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { p.bridge(dc) })
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-gathered
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, requester)
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(w).Encode(map[string]string{"sdp": pc.LocalDescription().SDP}); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := aw.Close(); err != nil {
		return "", err
	}
	log.Printf("answered an offer (%d bytes sealed)", buf.Len())
	return buf.String(), nil
}

// bridge carries raw TCP between a datachannel and the ssh port, as the
// guest agent does for 127.0.0.1:22.
func (p *peer) bridge(dc *webrtc.DataChannel) {
	log.Printf("datachannel %q", dc.Label())
	var conn net.Conn
	dc.OnOpen(func() {
		c, err := net.Dial("tcp", p.sshAddr)
		if err != nil {
			log.Printf("dial ssh: %v", err)
			_ = dc.Close()
			return
		}
		conn = c
		go func() {
			buf := make([]byte, 16384)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if err := dc.Send(buf[:n]); err != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			_ = dc.Close()
		}()
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if conn != nil {
			_, _ = conn.Write(m.Data)
		}
	})
	dc.OnClose(func() {
		if conn != nil {
			_ = conn.Close()
		}
	})
}

func (p *peer) serveSSH(ln net.Listener) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.authorized != nil && bytes.Equal(p.authorized.Marshal(), key.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	cfg.AddHostKey(p.hostKey)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
			if err != nil {
				log.Printf("ssh handshake: %v", err)
				return
			}
			log.Printf("ssh login %s", sc.User())
			go ssh.DiscardRequests(reqs)
			for nc := range chans {
				if nc.ChannelType() != "session" {
					_ = nc.Reject(ssh.UnknownChannelType, "session only")
					continue
				}
				ch, creqs, err := nc.Accept()
				if err != nil {
					continue
				}
				go session(ch, creqs)
			}
		}()
	}
}

func session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	var tty *os.File
	var cols, rows uint32 = 80, 24
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// string term, uint32 cols, uint32 rows, ...
			if n := binary.BigEndian.Uint32(req.Payload); len(req.Payload) >= int(4+n+8) {
				cols = binary.BigEndian.Uint32(req.Payload[4+n:])
				rows = binary.BigEndian.Uint32(req.Payload[8+n:])
			}
			_ = req.Reply(true, nil)
		case "window-change":
			if len(req.Payload) >= 8 && tty != nil {
				_ = pty.Setsize(tty, &pty.Winsize{Cols: uint16(binary.BigEndian.Uint32(req.Payload)), Rows: uint16(binary.BigEndian.Uint32(req.Payload[4:]))})
				log.Printf("window-change %dx%d", binary.BigEndian.Uint32(req.Payload), binary.BigEndian.Uint32(req.Payload[4:]))
			}
		case "shell":
			// A fixed command, never a user shell: a login shell's rc can
			// auto-attach tmux and hand the browser a live session. It
			// prints what the acceptance checks look for, then echoes.
			dir, err := os.MkdirTemp("", "testpeer-")
			if err != nil {
				_ = req.Reply(false, nil)
				return
			}
			defer os.RemoveAll(dir)
			cmd := exec.Command("/bin/sh", "-c", "id; uname -sm; stty size; exec cat")
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TERM=xterm-256color"}
			f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
			if err != nil {
				_ = req.Reply(false, nil)
				return
			}
			tty = f
			_ = req.Reply(true, nil)
			go func() { _, _ = io.Copy(f, ch) }()
			go func() {
				_, _ = io.Copy(ch, f)
				code := 0
				if err := cmd.Wait(); err != nil {
					code = 1
				}
				_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(code)))
				_ = ch.Close()
			}()
		default:
			_ = req.Reply(false, nil)
		}
	}
}
