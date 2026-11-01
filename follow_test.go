package kalathos_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kalathos "kalathos.ai/sdk-go"
	"kalathos.ai/sdk-go/internal/apiclient"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func mismatch(target string) *http.Response {
	return reply(http.StatusMisdirectedRequest, `{"error":{"type":"invalid_request_error","code":"region_mismatch","message":"wrong region","region":"eu","api_base_url":"`+target+`"}}`)
}

type fakeAPI struct {
	mu      sync.Mutex
	urls    []string
	auths   []string
	respond func(n int) *http.Response
}

func (f *fakeAPI) client() *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.urls = append(f.urls, r.URL.String())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		return f.respond(len(f.urls) - 1), nil
	})}
}

func (f *fakeAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

type errs struct {
	mu   sync.Mutex
	list []string
}

func (e *errs) add(err error) {
	e.mu.Lock()
	e.list = append(e.list, err.Error())
	e.mu.Unlock()
}

func (e *errs) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

func newReporter(t *testing.T, api *fakeAPI, e *errs) *kalathos.Client {
	t.Helper()
	c, err := kalathos.New(kalathos.Config{
		SecretKey: "kl_sk_test_x", FieldID: "fld_t", Endpoint: "https://api.test", FlushIntervalMs: 60_000,
		HTTPClient: api.client(), OnError: e.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func event() apiclient.ServerEvent {
	return apiclient.ServerEvent{Ts: 1, Request: apiclient.ServerEventRequest{Method: "GET", Url: "/", DurationMs: 1}}
}

func TestFollowsRegionMismatchOnce(t *testing.T) {
	const eu = "https://api.eu.kalathos.ai"
	api := &fakeAPI{respond: func(n int) *http.Response {
		if n == 0 {
			return mismatch(eu)
		}
		return reply(http.StatusAccepted, "{}")
	}}
	var e errs
	c := newReporter(t, api, &e)
	c.Record(event())
	_ = c.Flush(context.Background())
	want := []string{"https://api.test/v1/ingest/server-events", eu + "/v1/ingest/server-events"}
	if got := api.seen(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("urls = %v, want %v", got, want)
	}
	if c.Pending() != 0 {
		t.Fatalf("batch not delivered: %d pending", c.Pending())
	}
	c.Record(event())
	_ = c.Close(context.Background())
	if got := api.seen(); len(got) != 3 || got[2] != eu+"/v1/ingest/server-events" {
		t.Fatalf("later batches should go to the new endpoint: %v", got)
	}
	if msgs := e.all(); len(msgs) != 1 || !strings.Contains(msgs[0], "api.eu.kalathos.ai") {
		t.Fatalf("expected one log line naming the new host, got %v", msgs)
	}
	for _, a := range api.auths {
		if a != "Bearer kl_sk_test_x" {
			t.Fatalf("unexpected authorization %q", a)
		}
	}
}

func TestSecondRegionMismatchIsNotFollowed(t *testing.T) {
	api := &fakeAPI{respond: func(n int) *http.Response {
		if n == 0 {
			return mismatch("https://api.eu.kalathos.ai")
		}
		return mismatch("https://api.kalathos.ai")
	}}
	var e errs
	c := newReporter(t, api, &e)
	c.Record(event())
	_ = c.Flush(context.Background())
	if got := api.seen(); len(got) != 2 {
		t.Fatalf("want 2 requests, got %v", got)
	}
	if c.Pending() != 0 {
		t.Fatalf("a 4xx is dropped, got %d pending", c.Pending())
	}
}

func TestRegionMismatchToUntrustedTargetIsNotFollowed(t *testing.T) {
	for name, target := range map[string]string{
		"http":        "http://api.eu.kalathos.ai",
		"foreign":     "https://evil.example",
		"lookalike":   "https://api.eu.kalathos.ai.evil.example",
		"credentials": "https://user:pw@api.eu.kalathos.ai",
		"port":        "https://api.eu.kalathos.ai:8443",
		"path":        "https://api.eu.kalathos.ai/proxy",
		"empty":       "",
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{respond: func(int) *http.Response { return mismatch(target) }}
			var e errs
			c := newReporter(t, api, &e)
			c.Record(event())
			_ = c.Flush(context.Background())
			got := api.seen()
			if len(got) != 1 || !strings.HasPrefix(got[0], "https://api.test/") {
				t.Fatalf("must not leave the configured endpoint: %v", got)
			}
		})
	}
}

func TestOtherMisdirectedAnswerIsNotFollowed(t *testing.T) {
	api := &fakeAPI{respond: func(int) *http.Response {
		return reply(http.StatusMisdirectedRequest, `{"error":{"code":"other","api_base_url":"https://api.eu.kalathos.ai"}}`)
	}}
	var e errs
	c := newReporter(t, api, &e)
	c.Record(event())
	_ = c.Flush(context.Background())
	if got := api.seen(); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestSeedResolve402IsNoSeed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"payment_required"}}`))
	}))
	defer srv.Close()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	var e errs
	s, err := kalathos.NewFetcherSeeds(kalathos.FetcherSeedsConfig{
		PublishableKey: "kl_pk_test_x", FieldID: "fld_t", Origin: "https://shop.example", Endpoint: srv.URL,
		Routes: []kalathos.SeedRoute{{Match: "/products/*", Goal: "purchase"}}, CacheTTLMs: 150, OnError: e.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if html := s.Render(ctx, "/products/a"); html != "" {
			t.Fatalf("render %d: %q", i, html)
		}
	}
	if seed, err := s.Resolve(ctx, "purchase", "products_a"); seed != nil || err != nil {
		t.Fatalf("Resolve = %v, %v", seed, err)
	}
	if n := count(); n != 1 {
		t.Fatalf("a 402 must be remembered, got %d calls", n)
	}
	time.Sleep(200 * time.Millisecond)
	_ = s.Render(ctx, "/products/a") // stale: returns at once, refreshes in the background
	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("remembered only for a while, got %d calls", n)
	}
	if msgs := e.all(); len(msgs) != 0 {
		t.Fatalf("a 402 is not an error: %v", msgs)
	}
}
