package kalathos

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func repoOpenAPI(t *testing.T) *jsonschema.Schema {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "openapi.codegen.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("openapi.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("openapi.json#/components/schemas/ServerEventsBatch")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServerEventsBatchMatchesOpenAPI(t *testing.T) {
	schema := repoOpenAPI(t)
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, err := New(Config{
		SecretKey:       "kl_sk_test_x",
		FieldID:         "fld_t",
		Endpoint:        srv.URL,
		BatchSize:       1,
		FlushIntervalMs: 60_000,
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			var err error
			captured, err = readBody(req)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody, Header: make(http.Header)}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/products/a?utm_source=chatgpt", nil)
	req.Header.Set("User-Agent", "ChatGPT-User/1.0")
	req.Header.Set("Accept", "text/html")
	c.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	_ = c.Flush(context.Background())
	if len(captured) == 0 {
		t.Fatal("no body captured")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(captured))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(inst); err != nil {
		t.Fatal(err)
	}
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(req.Body)
	return buf.Bytes(), err
}
