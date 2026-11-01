package kalathos_test

import (
	"testing"

	kalathos "kalathos.ai/sdk-go"
)

func TestEndpointForKey(t *testing.T) {
	for key, want := range map[string]string{
		"kl_sk_live_eu_x": kalathos.EUEndpoint,
		"kl_sk_test_eu_x": kalathos.EUEndpoint,
		"kl_pk_live_eu_x": kalathos.EUEndpoint,
		"kl_pk_test_eu_x": kalathos.EUEndpoint,
		"kl_sk_live_x":    kalathos.DefaultEndpoint,
		"kl_sk_live_eux":  kalathos.DefaultEndpoint,
		"kl_sk_live_us_x": kalathos.DefaultEndpoint,
		"kl_sk_live_EU_x": kalathos.DefaultEndpoint,
		"":                kalathos.DefaultEndpoint,
	} {
		if got := kalathos.EndpointForKey(key); got != want {
			t.Errorf("EndpointForKey(%q) = %q, want %q", key, got, want)
		}
	}
}
