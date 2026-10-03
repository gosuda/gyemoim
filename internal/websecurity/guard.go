// Package websecurity contains reusable protections for the loopback management UI.
package websecurity

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const csrfHeader = "X-Gyemoim-CSRF"

// Guard validates the browser origin and host for management requests.
type Guard struct {
	csrf string
	host string
	port string
}

// New creates a guard for one exact local origin and listener port.
func New(port int, csrfToken string) *Guard {
	portText := strconv.Itoa(port)
	return &Guard{
		csrf: csrfToken,
		host: "127.0.0.1",
		port: portText,
	}
}

// Host restricts all requests to the IPv4 loopback address and actual listener port.
func (g *Guard) Host(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost, requestPort, err := net.SplitHostPort(r.Host)
		if err != nil && r.Host == g.host && g.port == "80" {
			requestHost, requestPort, err = g.host, "80", nil
		}
		ip := net.ParseIP(requestHost)
		if err != nil || ip == nil || !ip.Equal(net.ParseIP(g.host)) || requestPort != g.port {
			http.Error(w, "invalid Host header", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Management applies browser-origin protections to sensitive reads and every mutation.
// Future OAuth callback and bearer-authenticated /v1 routes should be routed outside it.
func (g *Guard) Management(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if isSafeMethod(r.Method) {
			if !g.validOriginIfPresent(r) || isCrossSite(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		} else {
			if !g.hasValidOrigin(r) || isCrossSite(r) || !g.validCSRF(r) {
				http.Error(w, "request origin or CSRF token is invalid", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) validOriginIfPresent(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || g.sameOrigin(origin)
}

func (g *Guard) hasValidOrigin(r *http.Request) bool {
	return g.sameOrigin(r.Header.Get("Origin"))
}

func (g *Guard) sameOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() != g.host || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	return port == g.port
}

func (g *Guard) validCSRF(r *http.Request) bool {
	provided := r.Header.Get(csrfHeader)
	return len(provided) == len(g.csrf) && subtle.ConstantTimeCompare([]byte(provided), []byte(g.csrf)) == 1
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func isCrossSite(r *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	return fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none"
}

func setSecurityHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; script-src 'self'; style-src 'self'; connect-src 'self'")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cache-Control", "no-store")
}
