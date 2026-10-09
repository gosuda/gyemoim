// Package websecurity contains reusable browser-origin protections for the management UI.
package websecurity

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const csrfHeader = "X-Gyemoim-CSRF"

// Guard validates the browser origin of management requests against the Host of
// each incoming request.
type Guard struct {
	csrf string
}

// New creates a guard with the given CSRF token.
func New(csrfToken string) *Guard {
	return &Guard{csrf: csrfToken}
}

// Callback adds response security headers without applying browser Origin or CSRF
// checks, which would reject the provider's top-level OAuth redirect.
func (g *Guard) Callback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		next.ServeHTTP(w, r)
	})
}

// Management applies browser-origin protections to sensitive reads and every mutation.
// Future OAuth callback and bearer-authenticated /v1 routes should be routed outside it.
func (g *Guard) Management(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if isSafeMethod(r.Method) {
			if !g.validOriginIfPresent(r) || (isCrossSite(r) && !isOAuthResultLanding(r)) {
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
	return origin == "" || g.sameOrigin(r, origin)
}

func (g *Guard) hasValidOrigin(r *http.Request) bool {
	return g.sameOrigin(r, r.Header.Get("Origin"))
}

// sameOrigin reports whether an Origin header refers to the request's own Host.
// The comparison is scheme-agnostic and covers only host:port: the gateway
// serves plain HTTP behind a reverse proxy while browsers send https origins.
// An absent port on either side (the default for the site's own scheme) counts
// as no port and matches only another portless value.
func (g *Guard) sameOrigin(r *http.Request, origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	requestHost, requestPort := splitHostPort(r.Host)
	return strings.EqualFold(parsed.Hostname(), requestHost) && parsed.Port() == requestPort
}

func splitHostPort(hostport string) (host string, port string) {
	if parsedHost, parsedPort, err := net.SplitHostPort(hostport); err == nil {
		return parsedHost, parsedPort
	}
	return hostport, ""
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

// isOAuthResultLanding allows only the fixed, read-only result redirect from the
// provider callback to survive Fetch Metadata's cross-site navigation marker.
func isOAuthResultLanding(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		return false
	}
	values := r.URL.Query()
	if len(values) != 1 || len(values["oauth_result"]) != 1 {
		return false
	}
	switch values.Get("oauth_result") {
	case "connected", "plan_usage_disabled", "require_reauthentication", "authorization_denied", "failed":
		return true
	default:
		return false
	}
}
