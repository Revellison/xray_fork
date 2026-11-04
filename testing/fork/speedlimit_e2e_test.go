// Package fork holds end-to-end regression tests for this fork's additions.
// See FORK_MAINTENANCE.md.
package fork

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/protocol/tls/cert"
	core "github.com/xtls/xray-core/core"
	_ "github.com/xtls/xray-core/main/distro/all"
)

const (
	limitKbps = 8000
	userID    = "11111111-2222-3333-4444-555555555555"
)

var nextUser atomic.Int64

type obj = map[string]any

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// servers runs an echo target, a bulk source and a counting sink.
type servers struct {
	echo, bulk, sink int
	received         atomic.Int64
	listeners        []net.Listener
}

func (s *servers) listen(t *testing.T, handle func(net.Conn)) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listeners = append(s.listeners, l)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.(*net.TCPConn).SetNoDelay(true)
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func startServers(t *testing.T) *servers {
	t.Helper()
	s := &servers{}
	s.echo = s.listen(t, func(c net.Conn) { io.Copy(c, c) })
	s.bulk = s.listen(t, func(c net.Conn) {
		b := make([]byte, 64*1024)
		for {
			if _, err := c.Write(b); err != nil {
				return
			}
		}
	})
	s.sink = s.listen(t, func(c net.Conn) {
		b := make([]byte, 64*1024)
		for {
			n, err := c.Read(b)
			s.received.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	t.Cleanup(func() {
		for _, l := range s.listeners {
			l.Close()
		}
	})
	return s
}

func startXray(t *testing.T, config obj) {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.LoadConfig("json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("load config: %v\n%s", err, raw)
	}
	x, err := core.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
}

type transportCase struct {
	name      string
	vless     bool
	udpServer bool
	server    obj
	client    obj
	// maxPingP95 allows for packet-up, whose post interval adds a fixed 30 ms or more.
	maxPingP95 time.Duration
}

func cases() []transportCase {
	c, _ := cert.MustGenerate(nil, cert.DNSNames("fork.test"))
	certPEM, keyPEM := c.ToPEM()
	sum := sha256.Sum256(c.Certificate)
	pin := hex.EncodeToString(sum[:])
	lines := func(b []byte) []string { return strings.Split(strings.TrimSpace(string(b)), "\n") }
	serverTLS := func(alpn string) obj {
		return obj{"alpn": []string{alpn}, "certificates": []obj{{"certificate": lines(certPEM), "key": lines(keyPEM)}}}
	}
	clientTLS := func(alpn string) obj {
		return obj{"serverName": "fork.test", "alpn": []string{alpn}, "pinnedPeerCertSha256": pin}
	}
	const fast, packet = 100 * time.Millisecond, 200 * time.Millisecond

	out := []transportCase{
		{name: "raw", vless: true, maxPingP95: fast,
			server: obj{"network": "tcp", "security": "none"}, client: obj{"network": "tcp", "security": "none"}},
		{name: "websocket", vless: true, maxPingP95: fast,
			server: obj{"network": "ws", "security": "none", "wsSettings": obj{"path": "/w"}},
			client: obj{"network": "ws", "security": "none", "wsSettings": obj{"path": "/w"}}},
	}
	for _, alpn := range []string{"h2", "h3"} {
		for _, mode := range []string{"stream-one", "stream-up", "packet-up"} {
			limit := fast
			if mode == "packet-up" {
				limit = packet
			}
			settings := obj{"path": "/x", "mode": mode}
			out = append(out, transportCase{
				name: "xhttp-" + alpn + "-" + mode, vless: true, udpServer: alpn == "h3", maxPingP95: limit,
				server: obj{"network": "xhttp", "security": "tls", "xhttpSettings": settings, "tlsSettings": serverTLS(alpn)},
				client: obj{"network": "xhttp", "security": "tls", "xhttpSettings": settings, "tlsSettings": clientTLS(alpn)},
			})
		}
	}
	out = append(out, transportCase{
		name: "hysteria2", udpServer: true, maxPingP95: fast,
		server: obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2}, "tlsSettings": serverTLS("h3")},
		client: obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2, "auth": "secret"}, "tlsSettings": clientTLS("h3")},
	})
	return out
}

func startProxy(t *testing.T, tc transportCase, s *servers) (echo, bulk, sink int) {
	t.Helper()
	// The limiter is per process and keyed by email. Data buffered by an earlier subtest
	// can still be draining, so sharing a user would let it use up this subtest's tokens.
	email := fmt.Sprintf("user%d@fork.test", nextUser.Add(1))
	proxyPort := freePort(t)
	inbound := obj{"listen": "127.0.0.1", "port": proxyPort, "streamSettings": tc.server}
	outbound := obj{"streamSettings": tc.client}
	if tc.vless {
		inbound["protocol"] = "vless"
		inbound["settings"] = obj{"clients": []obj{{"id": userID, "email": email}}, "decryption": "none"}
		outbound["protocol"] = "vless"
		outbound["settings"] = obj{"vnext": []obj{{"address": "127.0.0.1", "port": proxyPort,
			"users": []obj{{"id": userID, "encryption": "none"}}}}}
	} else {
		inbound["protocol"] = "hysteria"
		inbound["settings"] = obj{"version": 2, "clients": []obj{{"auth": "secret", "email": email}}}
		outbound["protocol"] = "hysteria"
		outbound["settings"] = obj{"version": 2, "address": "127.0.0.1", "port": proxyPort}
	}
	startXray(t, obj{
		"log":      obj{"loglevel": "none"},
		"inbounds": []obj{inbound},
		"outbounds": []obj{{"protocol": "freedom",
			"settings": obj{"finalRules": []obj{{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}}},
		"speedLimit": obj{"enabled": true, "default": obj{"down": limitKbps, "up": limitKbps}},
	})
	if tc.udpServer {
		time.Sleep(300 * time.Millisecond)
	}

	echo, bulk, sink = freePort(t), freePort(t), freePort(t)
	door := func(port, target int) obj {
		return obj{"listen": "127.0.0.1", "port": port, "protocol": "dokodemo-door",
			"settings": obj{"address": "127.0.0.1", "port": target, "network": "tcp"}}
	}
	startXray(t, obj{
		"log":       obj{"loglevel": "none"},
		"inbounds":  []obj{door(echo, s.echo), door(bulk, s.bulk), door(sink, s.sink)},
		"outbounds": []obj{outbound},
	})
	return echo, bulk, sink
}

// measure runs four bulk flows through port and pings on a separate flow of the same user.
func measure(t *testing.T, echoPort, bulkPort int, upload bool, counter *atomic.Int64) (mbps float64, pings []time.Duration) {
	t.Helper()
	const warmup, window = time.Second, 3 * time.Second
	stop := time.Now().Add(warmup + window + 500*time.Millisecond)
	for i := 0; i < 4; i++ {
		go func() {
			c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", bulkPort))
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(stop)
			b := make([]byte, 64*1024)
			for {
				if upload {
					if _, err = c.Write(b); err != nil {
						return
					}
				} else {
					n, err := c.Read(b)
					counter.Add(int64(n))
					if err != nil {
						return
					}
				}
			}
		}()
	}
	time.Sleep(warmup)

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.(*net.TCPConn).SetNoDelay(true)
	before, started := counter.Load(), time.Now()
	payload, reply := make([]byte, 32), make([]byte, 32)
	for time.Since(started) < window {
		t0 := time.Now()
		c.SetDeadline(t0.Add(5 * time.Second))
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("ping write after %d pings: %v", len(pings), err)
		}
		if _, err := io.ReadFull(c, reply); err != nil {
			t.Fatalf("ping stalled after %d pings: %v", len(pings), err)
		}
		pings = append(pings, time.Since(t0))
		time.Sleep(20 * time.Millisecond)
	}
	return float64(counter.Load()-before) * 8 / time.Since(started).Seconds() / 1e6, pings
}

// TestLimitHoldsAndPingStaysLow guards against two past bugs: limits that did nothing on
// DispatchLink paths, and throttled uploads that stalled other flows on a shared connection.
func TestLimitHoldsAndPingStaysLow(t *testing.T) {
	if testing.Short() {
		t.Skip("measures real traffic")
	}
	for _, tc := range cases() {
		for _, upload := range []bool{false, true} {
			name := tc.name + "/down"
			if upload {
				name = tc.name + "/up"
			}
			t.Run(name, func(t *testing.T) {
				s := startServers(t)
				echo, bulk, sink := startProxy(t, tc, s)
				var downloaded atomic.Int64
				port, counter := bulk, &downloaded
				if upload {
					port, counter = sink, &s.received
				}
				mbps, pings := measure(t, echo, port, upload, counter)
				sort.Slice(pings, func(i, j int) bool { return pings[i] < pings[j] })
				p95 := pings[len(pings)*95/100]
				t.Logf("%.1f Mbit/s, %d pings, p50 %v, p95 %v", mbps, len(pings), pings[len(pings)/2], p95)

				limit := float64(limitKbps) / 1000
				if mbps < limit*0.7 || mbps > limit*1.3 {
					t.Errorf("throughput %.1f Mbit/s, want about %.0f", mbps, limit)
				}
				if p95 > tc.maxPingP95 {
					t.Errorf("ping p95 %v under load, want at most %v", p95, tc.maxPingP95)
				}
			})
		}
	}
}
