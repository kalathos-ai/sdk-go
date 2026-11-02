package kalathos_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	kalathos "kalathos.ai/sdk-go"
)

func TestFetcherSeedsResolveServerSideWithTheSecretKey(t *testing.T) {
	var mu sync.Mutex
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Clone(context.Background())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = w.Write([]byte(`{"seed_id":"sd_01","variant":"a","html":"<section><p>Hi</p></section>","jsonld":{}}`))
	}))
	defer srv.Close()
	s, err := kalathos.NewFetcherSeeds(kalathos.FetcherSeedsConfig{
		SecretKey: "kl_sk_test_x", FieldID: "fld_t", Endpoint: srv.URL,
		Routes: []kalathos.SeedRoute{{Match: "/products/*", Goal: "purchase"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if html := s.Render(context.Background(), "/products/mug"); !strings.Contains(html, `data-seed-id="sd_01"`) {
		t.Fatalf("Render = %q", html)
	}
	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("the resolver was not called")
	}
	if got.URL.Path != "/v1/seeds/server-resolve" {
		t.Errorf("path = %s", got.URL.Path)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer kl_sk_test_x" {
		t.Errorf("Authorization = %q", a)
	}
	if o := got.Header.Get("Origin"); o != "" {
		t.Errorf("Origin = %q, want none", o)
	}
	q := got.URL.Query()
	if q.Get("field") != "fld_t" || q.Get("goal") != "purchase" || q.Get("archetype") != "products_mug" || q.Has("key") || q.Has("session") {
		t.Errorf("query = %v", q)
	}
}

func TestFetcherSeedsRequireASecretKey(t *testing.T) {
	for _, key := range []string{"", "kl_pk_live_x", "kl_pk_test_x", "sk_live_x"} {
		if _, err := kalathos.NewFetcherSeeds(kalathos.FetcherSeedsConfig{SecretKey: key, FieldID: "fld_t"}); err == nil || !strings.Contains(err.Error(), "kl_sk_") {
			t.Errorf("%q: err = %v", key, err)
		}
	}
	if _, err := kalathos.NewFetcherSeeds(kalathos.FetcherSeedsConfig{SecretKey: "kl_sk_live_x"}); err == nil {
		t.Error("FieldID is required")
	}
}
