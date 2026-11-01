package kalathos

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// DefaultAllowQuery lists query parameters kept in reported URLs.
var DefaultAllowQuery = []string{"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"}

const (
	maxHeaders       = 32
	maxHeaderValue   = 2048
	maxSignedHeaders = 16
)

var headerAllow = map[string]struct{}{
	"signature": {}, "signature-input": {}, "signature-agent": {},
	"from": {}, "x-agent": {}, "accept": {}, "accept-language": {}, "referer": {},
	"cf-verified-bot": {}, "cf-verified-bot-category": {}, "cf-bot-score": {}, "cf-ipcountry": {},
	"x-kalathos-cf-detection-ids": {}, "x-human-verdict": {},
}

var headerPrefixes = []string{"sec-ch-ua", "sec-fetch-"}

var signedDeny = map[string]struct{}{
	"cookie": {}, "authorization": {}, "proxy-authorization": {}, "set-cookie": {}, "user-agent": {},
}

// StaticAssetRE matches paths that are not worth reporting.
var StaticAssetRE = regexp.MustCompile(`\.(?:m?js|css|map|png|jpe?g|gif|svg|ico|webp|avif|bmp|woff2?|ttf|otf|eot|mp4|webm|mp3|ogg|wav|wasm)$`)

// HeaderAllowed reports whether a request header may be forwarded to kalathos.
func HeaderAllowed(name string) bool {
	n := strings.ToLower(name)
	if _, ok := headerAllow[n]; ok {
		return true
	}
	for _, p := range headerPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// PickHeaders copies allow-listed headers; cookies and authorization never leave the host.
func PickHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	n := 0
	for name, vals := range h {
		if n >= maxHeaders {
			break
		}
		if !HeaderAllowed(name) {
			continue
		}
		v := strings.TrimSpace(strings.Join(vals, ", "))
		if v == "" || len(v) > maxHeaderValue {
			continue
		}
		out[strings.ToLower(name)] = v
		n++
	}
	return out
}

var sigInputHeaderRe = regexp.MustCompile(`\(([^)]*)\)`)
var sigItemRe = regexp.MustCompile(`"([^"]*)"((?:;[^\s;]+)*)`)

// CoveredHeaders returns request header names covered by a Signature-Input value.
func CoveredHeaders(signatureInput string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, list := range sigInputHeaderRe.FindAllStringSubmatch(signatureInput, -1) {
		for _, item := range sigItemRe.FindAllStringSubmatch(list[1], -1) {
			name := strings.ToLower(item[1])
			params := strings.Split(item[2], ";")
			hasReq := false
			for _, p := range params {
				if strings.TrimSpace(strings.Split(p, "=")[0]) == "req" {
					hasReq = true
					break
				}
			}
			if name == "" || strings.HasPrefix(name, "@") || hasReq {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// PickSignedHeaders forwards signature-covered headers beyond the allow-list.
func PickSignedHeaders(h http.Header) map[string]string {
	sig := h.Get("Signature")
	input := h.Get("Signature-Input")
	if sig == "" || input == "" {
		return nil
	}
	out := make(map[string]string)
	n := 0
	for _, name := range CoveredHeaders(input) {
		if n >= maxSignedHeaders {
			break
		}
		if _, deny := signedDeny[name]; deny {
			continue
		}
		if strings.HasPrefix(name, "x-kalathos-") || HeaderAllowed(name) {
			continue
		}
		v := strings.TrimSpace(h.Get(name))
		if v == "" || len(v) > maxHeaderValue {
			continue
		}
		out[name] = v
		n++
	}
	return out
}

// IsSigned reports whether the request carries a Web Bot Auth signature.
func IsSigned(h http.Header) bool {
	return h.Get("Signature") != "" && h.Get("Signature-Input") != ""
}

// TargetURI is the absolute request URL as received (scheme://host/path?query).
func TargetURI(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = r.Header.Get(":authority")
	}
	if host == "" {
		return ""
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	} else if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		first := strings.TrimSpace(strings.Split(xfp, ",")[0])
		first = strings.ToLower(first)
		if first == "https" || first == "http" {
			proto = first
		}
	}
	path := r.RequestURI
	if path == "" && r.URL != nil {
		path = r.URL.RequestURI()
	}
	if path == "" {
		path = "/"
	}
	uri := proto + "://" + host + path
	if len(uri) > maxHeaderValue {
		return ""
	}
	return uri
}

// IsStaticAsset reports paths that are not worth reporting.
func IsStaticAsset(path string) bool {
	return StaticAssetRE.MatchString(path)
}

// CleanURL splits raw into path and URL with allow-listed query parameters.
func CleanURL(raw string, allowQuery []string) (path, cleaned string) {
	q := strings.IndexByte(raw, '?')
	if q < 0 {
		return raw, raw
	}
	path = raw[:q]
	if len(allowQuery) == 0 {
		return path, path
	}
	in, err := url.ParseQuery(raw[q+1:])
	if err != nil {
		return path, path
	}
	kept := url.Values{}
	for k, vs := range in {
		if !containsString(allowQuery, k) {
			continue
		}
		for _, v := range vs {
			kept.Add(k, v)
		}
	}
	qs := kept.Encode()
	if qs == "" {
		return path, path
	}
	return path, path + "?" + qs
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RemoteAddr returns the TCP peer address without an IPv4-mapped IPv6 prefix.
func RemoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		a := r.RemoteAddr
		if strings.HasPrefix(a, "::ffff:") && strings.Contains(a, ".") {
			return a[7:]
		}
		return a
	}
	if strings.HasPrefix(host, "::ffff:") && strings.Contains(host, ".") {
		return host[7:]
	}
	return host
}

// ForwardedForResolver builds a client-IP resolver for apps behind reverse proxies.
func ForwardedForResolver(isTrustedProxy func(string) bool) func(*http.Request) string {
	return func(r *http.Request) string {
		peer := RemoteAddr(r)
		if peer == "" || !isTrustedProxy(peer) {
			return peer
		}
		raw := r.Header.Get("X-Forwarded-For")
		hops := strings.Split(raw, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			if hop == "" {
				continue
			}
			if !isTrustedProxy(hop) {
				return hop
			}
		}
		return peer
	}
}

// IsPrivateAddress reports loopback and RFC1918 / ULA addresses.
func IsPrivateAddress(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	if parsed.IsLoopback() {
		return true
	}
	if parsed.To4() != nil {
		v4 := parsed.To4()
		switch {
		case v4[0] == 10:
			return true
		case v4[0] == 127:
			return true
		case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
			return true
		case v4[0] == 192 && v4[1] == 168:
			return true
		default:
			return false
		}
	}
	// ULA fc00::/7
	return len(parsed) == net.IPv6len && (parsed[0]&0xfe == 0xfc)
}
