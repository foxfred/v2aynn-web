package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"v2aynn-web/internal/config"
)

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func startTCPServer(t *testing.T, tlsMode bool) (string, func()) {
	t.Helper()
	var ln net.Listener
	var err error
	if tlsMode {
		cert := selfSignedCert(t)
		ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 64)
				_, _ = conn.Read(buf)
				_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = conn.Write([]byte("hp"))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestProbeReachableTCP(t *testing.T) {
	addr, stop := startTCPServer(t, false)
	defer stop()
	h, p, _ := net.SplitHostPort(addr)
	n := config.Node{Server: h, Port: p, Protocol: "vless"}
	if ms, ok := probeNode(n, 1500*time.Millisecond); !ok || ms <= 0 {
		t.Fatalf("可达TCP被误判: ok=%v ms=%d", ok, ms)
	}
}

func TestProbeReachableTLS(t *testing.T) {
	addr, stop := startTCPServer(t, true)
	defer stop()
	h, p, _ := net.SplitHostPort(addr)
	n := config.Node{Server: h, Port: p, Protocol: "vless", TLS: "tls", SNI: "localhost"}
	if ms, ok := probeNode(n, 1500*time.Millisecond); !ok || ms <= 0 {
		t.Fatalf("可达TLS被误判: ok=%v ms=%d", ok, ms)
	}
}

func TestProbeUnreachable(t *testing.T) {
	n := config.Node{Server: "127.0.0.1", Port: "1", Protocol: "vless"}
	if _, ok := probeNode(n, 800*time.Millisecond); ok {
		t.Fatal("不可达被误判可达")
	}
}