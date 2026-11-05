package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestIsDeniedAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":              true,
		"127.255.255.254":        true,
		"10.1.2.3":               true,
		"172.16.0.1":             true,
		"172.31.255.255":         true,
		"192.168.1.1":            true,
		"169.254.169.254":        true,
		"0.0.0.0":                true,
		"0.1.2.3":                true,
		"::1":                    true,
		"::":                     true,
		"fc00::1":                true,
		"fd12:3456::1":           true,
		"fe80::1":                true,
		"febf::1":                true,
		"::ffff:127.0.0.1":       true,
		"::ffff:169.254.169.254": true,
		"::ffff:10.0.0.1":        true,

		"8.8.8.8":              false,
		"172.15.255.255":       false,
		"172.32.0.0":           false,
		"192.169.0.1":          false,
		"1.1.1.1":              false,
		"2001:4860:4860::8888": false,
		"fec0::1":              false,
		"::ffff:8.8.8.8":       false,
	}
	for s, want := range cases {
		if got := isDeniedAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("isDeniedAddr(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestSafeClientDeniesResolvedLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached loopback server")
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	client := newSafeClient()

	for _, u := range []string{
		srv.URL,
		"http://localhost:" + port + "/",
		"http://[::ffff:127.0.0.1]:" + port + "/",
		"http://0.0.0.0:" + port + "/",
		"http://[::]:" + port + "/",
	} {
		_, err := client.Get(u)
		if !errors.Is(err, errDeniedAddress) {
			t.Errorf("GET %s: err = %v, want errDeniedAddress", u, err)
		}
	}
}

func TestSafeClientDeniesRedirectToLoopback(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect reached loopback server")
	}))
	defer internal.Close()

	// 公開ホストから内部へリダイレクトされる状況を、最初の接続だけ許可するクライアントで再現する
	client := newSafeClient()
	transport := client.Transport.(*http.Transport)
	safeDial := transport.DialContext
	first := true
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if first {
			first = false
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return safeDial(ctx, network, addr)
	}

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := client.Get(redirector.URL)
	if !errors.Is(err, errDeniedAddress) {
		t.Errorf("err = %v, want errDeniedAddress", err)
	}
}
