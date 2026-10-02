// [POS]: 重定向阻断、DNS/IP 固定及 TLS 原始主机名校验回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundDialPinsValidatedAddresses(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "")
	for _, test := range []struct {
		name    string
		ips     []string
		blocked bool
	}{
		{"public IPv4", []string{"8.8.8.8"}, false},
		{"public IPv6", []string{"2606:4700:4700::1111"}, false},
		{"private DNS", []string{"127.0.0.1"}, true},
		{"mixed DNS", []string{"8.8.8.8", "10.0.0.1"}, true},
		{"IPv6 private", []string{"fd00::1"}, true},
		{"mapped IPv4", []string{"::ffff:127.0.0.1"}, true},
		{"metadata", []string{"169.254.169.254"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, dials := 0, 0
			d := outboundDialer{
				lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
					lookups++
					var ips []net.IPAddr
					for _, raw := range test.ips {
						ips = append(ips, net.IPAddr{IP: net.ParseIP(raw)})
					}
					return ips, nil
				},
				dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					dials++
					if address != net.JoinHostPort(test.ips[0], "443") {
						t.Fatalf("unvalidated address dialed: %s", address)
					}
					a, b := net.Pipe()
					t.Cleanup(func() { a.Close(); b.Close() })
					return a, nil
				},
			}
			_, err := d.dialContext(context.Background(), "tcp", "webhook.example:443")
			if lookups != 1 || (test.blocked && (!errors.Is(err, ErrPrivateOutbound) || dials != 0)) || (!test.blocked && (err != nil || dials != 1)) {
				t.Fatalf("lookups=%d dials=%d err=%v", lookups, dials, err)
			}
		})
	}
}

func TestOutboundDNSFailureAndCancellation(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := outboundDialer{
		lookup: func(ctx context.Context, _ string) ([]net.IPAddr, error) { return nil, ctx.Err() },
		dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dial after DNS failure")
			return nil, nil
		},
	}
	if _, err := d.dialContext(ctx, "tcp", "webhook.example:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestOutboundRedirectRejectsPrivateBeforeSending(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "")
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer sink.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, status) }))
		client := NewOutboundHTTPClient(time.Second)
		// Simulate a public source using only a local socket; the destination policy stays real.
		client.Transport.(outboundTransport).transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(source.URL, "http://"))
		}
		resp, err := client.Post("http://8.8.8.8/hook", "application/json", strings.NewReader(`{"secret":"content"}`))
		if resp != nil {
			resp.Body.Close()
		}
		client.CloseIdleConnections()
		source.Close()
		if !errors.Is(err, ErrPrivateOutbound) || hits.Load() != 0 {
			t.Fatalf("status=%d err=%v sink hits=%d", status, err, hits.Load())
		}
	}
}

func TestOutboundAllowsPublicRedirectAndLimitsLoops(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "")
	var hits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loop" {
			http.Redirect(w, r, "/loop", 307)
			return
		}
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "http://1.1.1.1/done", 307)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Host != "1.1.1.1" || r.Method != "POST" || string(body) != "payload" {
			t.Errorf("redirect changed request: host=%s method=%s body=%s", r.Host, r.Method, body)
		}
		hits.Add(1)
	}))
	defer source.Close()
	client := NewOutboundHTTPClient(time.Second)
	defer client.CloseIdleConnections()
	client.Transport.(outboundTransport).transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(source.URL, "http://"))
	}
	resp, err := client.Post("http://8.8.8.8/hook", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("public redirect hits=%d", hits.Load())
	}
	resp, err = client.Get("http://8.8.8.8/loop")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "重定向次数") {
		t.Fatalf("unbounded redirects: %v", err)
	}
}

func TestOutboundPrivateOptInUsesDirectConnections(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "true")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer sink.Close()
	client := NewOutboundHTTPClient(time.Second)
	defer client.CloseIdleConnections()
	resp, err := client.Get(sink.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if err := ValidateOutboundPublicURL("file:///etc/passwd"); err == nil {
		t.Fatal("opt-in allowed non HTTP protocol")
	}
}

func TestOutboundPinnedIPRetainsTLSHostnameVerification(t *testing.T) {
	t.Setenv(AllowPrivateOutboundEnv, "")
	receivedSNI := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSNI <- r.TLS.ServerName
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	certificate := srv.Certificate()
	if len(certificate.DNSNames) == 0 {
		t.Fatal("TLS fixture has no DNS identity")
	}
	host := certificate.DNSNames[0]
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	client := NewOutboundHTTPClient(time.Second)
	defer client.CloseIdleConnections()
	tr := client.Transport.(outboundTransport).transport
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	d := outboundDialer{
		lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				t.Errorf("address not pinned: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "https://"))
		},
	}
	tr.DialContext = d.dialContext
	resp, err := client.Get("https://" + host + "/hook")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sni := <-receivedSNI; sni != host {
		t.Fatalf("TLS used pinned IP instead of hostname: %s", sni)
	}
	resp, err = client.Get("https://wrong-identity.example/hook")
	if resp != nil {
		resp.Body.Close()
	}
	var hostnameError x509.HostnameError
	if !errors.As(err, &hostnameError) {
		t.Fatalf("TLS identity mismatch accepted: %v", err)
	}
}
