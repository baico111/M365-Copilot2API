package outbound

import "strings"

// isProxyError reports whether an error is a proxy transport error (rather than
// an upstream application/quota error). It is used to keep proxy failures from
// affecting account health (account isolation).
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "quota_429") || strings.Contains(s, "overload_503") || strings.Contains(s, "auth_expired_401") || strings.Contains(s, "forbidden_403") || strings.Contains(s, "upstream 429") || strings.Contains(s, "upstream 401") || strings.Contains(s, "upstream 403") || strings.Contains(s, "upstream 503") {
		return false
	}
	if strings.Contains(s, "429") && strings.Contains(s, "upstream") {
		return false
	}
	if strings.Contains(s, "socks") || strings.Contains(s, "no such host") || strings.Contains(s, "dns") && !strings.Contains(s, "limited") || strings.Contains(s, "tls") || strings.Contains(s, "certificate") || strings.Contains(s, "x509") || strings.Contains(s, "handshake") || strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") || strings.Contains(s, "unexpected eof") || strings.Contains(s, "use of closed network connection") || strings.Contains(s, "network is unreachable") || strings.Contains(s, "ws_read_timeout") || strings.Contains(s, "ws_handshake") {
		return true
	}
	if strings.Contains(s, "proxy connect") || strings.Contains(s, "socks5") {
		return true
	}
	return false
}

// IsProxyIsolated reports whether the error is a proxy transport error that
// should not affect account health (account isolation).
func IsProxyIsolated(err error) bool { return isProxyError(err) }
