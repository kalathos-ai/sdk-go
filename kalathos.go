// Package kalathos is the Go server SDK: net/http middleware that reports agent traffic to
// kalathos.ai, an OpenAPI client, and Fetcher Seeds helpers.
package kalathos

import "regexp"

// DefaultEndpoint is the production API base URL (the US region).
const DefaultEndpoint = "https://api.kalathos.ai"

// EUEndpoint is the API base URL of the EU data region.
const EUEndpoint = "https://api.eu.kalathos.ai"

var euKey = regexp.MustCompile(`^kl_(pk|sk)_(live|test)_eu_`)

// EndpointForKey is the regional API a key belongs to: keys of an org in the EU region carry an
// eu_ marker after the type prefix (kl_sk_live_eu_…); every other key is a US key.
func EndpointForKey(key string) string {
	if euKey.MatchString(key) {
		return EUEndpoint
	}
	return DefaultEndpoint
}
