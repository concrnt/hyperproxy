package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var denyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	// Linux では :: への接続がループバックに落ちるので 0.0.0.0/8 と同様に拒否する
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

var errDeniedAddress = errors.New("destination address is in deny list")

func isDeniedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range denyPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// DNS 解決後・接続直前のアドレスを検査するので、リダイレクト先や DNS rebinding も弾ける
func denyControl(network, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", errDeniedAddress, address)
	}
	if isDeniedAddr(addrPort.Addr()) {
		return fmt.Errorf("%w: %s", errDeniedAddress, addrPort.Addr())
	}
	return nil
}

func newSafeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   denyControl,
	}
	transport := &http.Transport{
		// プロキシ経由だと検査対象がプロキシの IP になってしまうので使わない
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}
}
