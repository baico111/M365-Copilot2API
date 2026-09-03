package chathub

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// validateRemoteDownloadURL blocks SSRF: only https and public routable
// addresses are accepted, with a lookup-time recheck against private,
// loopback, link-local and cloud metadata ranges.
func validateRemoteDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid attachment URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("attachment download requires https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("attachment URL has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("attachment host does not resolve")
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return nil
}

func ipUnsafe(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 link-local is covered above on Go >= 1.17;
		// 100.64.0.0/10 (CGNAT) is not private per IP.IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	// 169.254.169.254 cloud metadata is link-local; belt and braces.
	if strings.HasPrefix(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}

// validatedDialContext fixes the DNS-rebinding (TOCTOU) hole in
// validateRemoteDownloadURL: validating the resolved IPs and then calling a
// plain http.Get re-resolves the hostname, letting a short-TTL record flip
// from a public IP to 127.0.0.1/169.254.169.254 between check and connect.
// This dialer re-resolves at connect time, drops every unsafe address, and
// dials the validated IP directly (TLS SNI/verification still uses the real
// hostname because http.Transport derives it from the request URL).
func validatedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// Literal IPs skip resolution.
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		resolved, rerr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if rerr != nil || len(resolved) == 0 {
			return nil, fmt.Errorf("attachment host does not resolve")
		}
		for _, ra := range resolved {
			ips = append(ips, ra.IP)
		}
	}
	var lastErr error
	for _, ip := range ips {
		if ipUnsafe(ip) {
			lastErr = fmt.Errorf("attachment URL targets a non-public address")
			continue
		}
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr != nil {
			lastErr = derr
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no safe address for attachment host")
	}
	return nil, lastErr
}
