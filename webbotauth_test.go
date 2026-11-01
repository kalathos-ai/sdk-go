package kalathos

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kalathos.ai/sdk-go/internal/apiclient"
)

const sigInput = `sig1=("@authority" "@method" "@path" "@query" "signature-agent";key="sig1" "x-intent" "accept" "cookie" "content-digest";req)` +
	`;created=1735689600;expires=1735693200;keyid="poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U";tag="web-bot-auth"`

func TestCoveredHeaders(t *testing.T) {
	got := CoveredHeaders(sigInput)
	want := []string{"signature-agent", "x-intent", "accept", "cookie"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestPickSignedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Signature-Input", sigInput)
	h.Set("Signature", "sig1=:AAAA:")
	h.Set("X-Intent", "browse")
	h.Set("Cookie", "sid=1")
	h.Set("Accept", "text/html")
	got := PickSignedHeaders(h)
	if got["x-intent"] != "browse" {
		t.Fatalf("got %v", got)
	}
	h.Del("Signature")
	if len(PickSignedHeaders(h)) != 0 {
		t.Fatal("expected empty without signature")
	}
}

func TestTargetURI(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/p?q=1&utm_source=x", nil)
	req.Host = "shop.example"
	if got := TargetURI(req); got != "http://shop.example/p?q=1&utm_source=x" {
		t.Fatalf("got %q", got)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/p?q=1&utm_source=x", nil)
	req2.Host = "shop.example"
	req2.TLS = &tls.ConnectionState{}
	if got := TargetURI(req2); got != "https://shop.example/p?q=1&utm_source=x" {
		t.Fatalf("got %q", got)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/p?q=1", nil)
	req3.Host = "shop.example"
	req3.Header.Set("X-Forwarded-Proto", "https, http")
	if got := TargetURI(req3); got != "https://shop.example/p?q=1" {
		t.Fatalf("got %q", got)
	}
}

type batchCapture struct {
	events [][]apiclient.ServerEvent
}

func (b *batchCapture) transport() http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var batch apiclient.ServerEventsBatch
		_ = json.Unmarshal(body, &batch)
		if batch.Events != nil {
			b.events = append(b.events, *batch.Events)
		}
		return jsonResponse(`{}`, http.StatusAccepted), nil
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(body string, code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestMiddlewareSignedRequest(t *testing.T) {
	var batches batchCapture
	c, err := New(Config{
		SecretKey:       "kl_sk_test_x",
		FieldID:         "fld_t",
		Endpoint:        "https://api.test",
		FlushIntervalMs: 20,
		HTTPClient:      &http.Client{Transport: batches.transport()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	app := httptest.NewServer(c.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})))
	defer app.Close()

	req, _ := http.NewRequest(http.MethodGet, app.URL+"/products/a?ref=chatgpt&utm_source=chatgpt", nil)
	req.Header.Set("Signature-Agent", `sig1="https://chatgpt.com"`)
	req.Header.Set("Signature-Input", sigInput)
	req.Header.Set("Signature", "sig1=:AAAA:")
	req.Header.Set("X-Intent", "browse")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Cookie", "sid=secret")
	req.Header.Set("Accept", "text/html")
	_, _ = http.DefaultClient.Do(req)

	req2, _ := http.NewRequest(http.MethodGet, app.URL+"/products/b", nil)
	req2.Header.Set("X-Intent", "browse")
	_, _ = http.DefaultClient.Do(req2)

	_ = c.Close(context.Background())
	if len(batches.events) < 1 {
		t.Fatal("expected batches")
	}
	var flat []apiclient.ServerEvent
	for _, b := range batches.events {
		flat = append(flat, b...)
	}
	if len(flat) < 2 {
		t.Fatalf("events %d", len(flat))
	}
	signed, plain := flat[0], flat[1]
	if !strings.Contains(signed.Request.Url, "/products/a?utm_source=chatgpt") {
		t.Fatalf("url %q", signed.Request.Url)
	}
	if signed.Request.TargetUri == nil || !strings.Contains(*signed.Request.TargetUri, "ref=chatgpt") {
		t.Fatalf("target_uri %v", signed.Request.TargetUri)
	}
	h := *signed.Request.Headers
	if h["x-intent"] != "browse" {
		t.Fatalf("headers %v", h)
	}
	if _, ok := h["cookie"]; ok {
		t.Fatalf("cookie must not be forwarded: %v", h)
	}
	if plain.Request.TargetUri != nil {
		t.Fatal("plain should not have target_uri")
	}
}
