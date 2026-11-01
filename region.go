package kalathos

import (
	"encoding/json"
	"net/url"
	"strings"
)

// regionHosts are the regional kalathos APIs a region_mismatch answer may point at.
var regionHosts = map[string]struct{}{}

func init() {
	for _, e := range []string{DefaultEndpoint, EUEndpoint} {
		if u, err := url.Parse(e); err == nil {
			regionHosts[u.Hostname()] = struct{}{}
		}
	}
}

// regionRedirect is the API base URL a 421 region_mismatch body names, or "" when the body is
// something else, names a host that is not a regional kalathos API (it must be https without
// credentials, port or path), or names the base URL already in use.
func regionRedirect(body []byte, current string) string {
	var e struct {
		Error struct {
			Code       string `json:"code"`
			APIBaseURL string `json:"api_base_url"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Code != "region_mismatch" {
		return ""
	}
	u, err := url.Parse(e.Error.APIBaseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" ||
		u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := regionHosts[host]; !ok {
		return ""
	}
	if cur, err := url.Parse(current); err == nil && cur.Scheme == "https" && strings.EqualFold(cur.Host, host) {
		return ""
	}
	return "https://" + host
}
