package kalathos_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	kalathos "kalathos.ai/sdk-go"
)

func TestMiddlewareRejectsPublishableKey(t *testing.T) {
	if _, err := kalathos.Middleware(kalathos.Config{SecretKey: "kl_pk_live_x", FieldID: "fld"}); !errors.Is(err, kalathos.ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
}

func TestMiddlewarePassesThrough(t *testing.T) {
	mw, err := kalathos.Middleware(kalathos.Config{SecretKey: "kl_sk_test_x", FieldID: "fld_t"})
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestMiddlewareWithCloseFlushesOnShutdown(t *testing.T) {
	var mu sync.Mutex
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, e := range body.Events {
			got = append(got, e.Request.URL)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer api.Close()
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}

	mw, closeFn, err := kalathos.MiddlewareWithClose(kalathos.Config{
		SecretKey: "kl_sk_test_x", FieldID: "fld_t", Endpoint: api.URL, FlushIntervalMs: 60_000, BatchSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://shop.example/products/a", nil))
	if n := len(sent()); n != 0 {
		t.Fatalf("event sent before Close: %d", n)
	}
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := sent(); len(s) != 1 || s[0] != "http://shop.example/products/a" {
		t.Fatalf("Close did not flush the pending event: %v", s)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://shop.example/after", nil))
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(sent()); n != 1 {
		t.Fatalf("requests after Close are not recorded; got %d events", n)
	}

	if _, _, err := kalathos.MiddlewareWithClose(kalathos.Config{SecretKey: "kl_pk_live_x", FieldID: "fld"}); !errors.Is(err, kalathos.ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
}

func TestMiddlewareReportsImplicitOK(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				Request map[string]any `json:"request"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, e := range body.Events {
			got = append(got, e.Request)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer api.Close()
	mw, closeFn, err := kalathos.MiddlewareWithClose(kalathos.Config{SecretKey: "kl_sk_test_x", FieldID: "fld_t", Endpoint: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://shop.example/empty", nil))
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("a handler that writes nothing is answered 200 by net/http: %v", got)
	}
}

func TestMiddlewareDoesNotSwallowHandlerPanics(t *testing.T) {
	var mu sync.Mutex
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, e := range body.Events {
			got = append(got, e.Request.URL)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer api.Close()
	mw, closeFn, err := kalathos.MiddlewareWithClose(kalathos.Config{SecretKey: "kl_sk_test_x", FieldID: "fld_t", Endpoint: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if p, _ := recover().(error); !errors.Is(p, http.ErrAbortHandler) {
				t.Fatalf("handler panic must reach the caller, got %v", p)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://shop.example/boom", nil))
	}()
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "http://shop.example/boom" {
		t.Fatalf("a request whose handler panicked is still recorded: %v", got)
	}
}

func TestMiddlewareReportsItsOwnPanics(t *testing.T) {
	var reported []error
	mw, err := kalathos.Middleware(kalathos.Config{
		SecretKey: "kl_sk_test_x", FieldID: "fld_t",
		Skip:    func(*http.Request) bool { panic("skip") },
		OnError: func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("request must still be served, got %d", rec.Code)
	}
	if len(reported) != 1 {
		t.Fatalf("want 1 reported error, got %v", reported)
	}
}
