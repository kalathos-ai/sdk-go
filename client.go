package kalathos

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"kalathos.ai/sdk-go/internal/apiclient"
)

const (
	// ServerEventsPath is POST /v1/ingest/server-events.
	ServerEventsPath = "/v1/ingest/server-events"
	// MaxBatch is the maximum events per ingest request.
	MaxBatch = 500
)

// Config configures request reporting.
type Config struct {
	SecretKey string
	FieldID   string
	Endpoint  string

	BatchSize       int
	FlushIntervalMs int
	MaxQueue        int
	TimeoutMs       int

	GetClientIP      func(*http.Request) string
	GetSessionHint   func(*http.Request) string
	SkipStaticAssets *bool
	Skip             func(*http.Request) bool
	AllowQuery       []string
	OnError          func(error)
	GetTargetURI     func(*http.Request) string

	HTTPClient *http.Client
}

// ErrInvalidKey is returned for keys that are not secret keys.
var ErrInvalidKey = errors.New("kalathos: SecretKey must start with kl_sk_live_ or kl_sk_test_")

func (c Config) validate() error {
	if !strings.HasPrefix(c.SecretKey, "kl_sk_live_") && !strings.HasPrefix(c.SecretKey, "kl_sk_test_") {
		return ErrInvalidKey
	}
	if c.FieldID == "" {
		return errors.New("kalathos: FieldID is required")
	}
	return nil
}

// Client reports server-side requests to kalathos.ai.
type Client struct {
	cfg        Config
	endpoint   string
	api        *apiclient.Client
	httpClient *http.Client
	auth       string
	followed   bool

	batchSize     int
	flushInterval time.Duration
	maxQueue      int
	timeout       time.Duration
	allowQuery    []string
	skipStatic    bool

	mu       sync.Mutex
	queue    []apiclient.ServerEvent
	inflight bool
	retryAt  time.Time
	backoff  time.Duration
	closed   bool
	timer    *time.Timer
}

const retryMax = 60 * time.Second

func retryable(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

// New builds a reporting client.
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	if base == "" {
		base = EndpointForKey(cfg.SecretKey)
	}
	apiBase := base
	if !strings.HasSuffix(apiBase, "/") {
		apiBase += "/"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	api, err := apiclient.NewClient(apiBase, apiclient.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	if batchSize > MaxBatch {
		batchSize = MaxBatch
	}
	flushMs := cfg.FlushIntervalMs
	if flushMs <= 0 {
		flushMs = 2000
	}
	if flushMs < 10 {
		flushMs = 10
	}
	maxQueue := cfg.MaxQueue
	if maxQueue <= 0 {
		maxQueue = 5000
	}
	if maxQueue < batchSize {
		maxQueue = batchSize
	}
	timeoutMs := cfg.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	allowQuery := cfg.AllowQuery
	if allowQuery == nil {
		allowQuery = DefaultAllowQuery
	}
	skipStatic := true
	if cfg.SkipStaticAssets != nil {
		skipStatic = *cfg.SkipStaticAssets
	}

	c := &Client{
		cfg:           cfg,
		endpoint:      base + ServerEventsPath,
		api:           api,
		httpClient:    httpClient,
		auth:          "Bearer " + cfg.SecretKey,
		batchSize:     batchSize,
		flushInterval: time.Duration(flushMs) * time.Millisecond,
		maxQueue:      maxQueue,
		timeout:       time.Duration(timeoutMs) * time.Millisecond,
		allowQuery:    allowQuery,
		skipStatic:    skipStatic,
	}
	c.timer = time.AfterFunc(c.flushInterval, c.tickFlush)
	return c, nil
}

func (c *Client) report(err error) {
	if err == nil || c.cfg.OnError == nil {
		return
	}
	defer func() { _ = recover() }()
	c.cfg.OnError(err)
}

func (c *Client) tickFlush() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.timer.Reset(c.flushInterval)
	need := len(c.queue) > 0
	c.mu.Unlock()
	if need {
		_ = c.Flush(context.Background())
	}
}

func (c *Client) enqueue(evs []apiclient.ServerEvent, front bool) {
	if front {
		c.queue = append(evs, c.queue...)
	} else {
		c.queue = append(c.queue, evs...)
	}
	if len(c.queue) > c.maxQueue {
		dropped := len(c.queue) - c.maxQueue
		c.report(errors.New("kalathos: queue full, dropped " + itoa(dropped) + " events"))
		c.queue = c.queue[len(c.queue)-c.maxQueue:]
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (c *Client) sendBatch(ctx context.Context, batch []apiclient.ServerEvent) bool {
	ok, moved := c.post(ctx, batch)
	if moved {
		ok, _ = c.post(ctx, batch)
	}
	return ok
}

// post sends one batch. moved: the API named another region's endpoint, now adopted; send the
// batch again.
func (c *Client) post(ctx context.Context, batch []apiclient.ServerEvent) (ok, moved bool) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	params := &apiclient.IngestServerEventsParams{Authorization: &c.auth}
	body := apiclient.ServerEventsBatch{Events: &batch}
	c.mu.Lock()
	api := c.api
	c.mu.Unlock()
	res, err := api.IngestServerEvents(ctx, params, body)
	if err != nil {
		c.report(err)
		return false, false
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return true, false
	}
	if res.StatusCode == http.StatusMisdirectedRequest {
		if host := c.followRegion(text); host != "" {
			c.report(errors.New("kalathos: region mismatch, sending server events to " + host + " from now on"))
			return false, true
		}
	}
	err = errors.New("kalathos: server-events responded " + res.Status)
	c.report(err)
	return !retryable(res.StatusCode), false // a 4xx is dropped: it would fail the same way again
}

// followRegion switches to the endpoint a region_mismatch body names, at most once per client,
// and returns its host ("" when it did not switch).
func (c *Client) followRegion(body []byte) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.followed {
		return ""
	}
	origin := regionRedirect(body, c.endpoint)
	if origin == "" {
		return ""
	}
	api, err := apiclient.NewClient(origin+"/", apiclient.WithHTTPClient(c.httpClient))
	if err != nil {
		return ""
	}
	c.followed = true
	c.api = api
	c.endpoint = origin + ServerEventsPath
	return strings.TrimPrefix(origin, "https://")
}

func (c *Client) drain(ctx context.Context, force bool) {
	for {
		c.mu.Lock()
		if c.inflight {
			c.mu.Unlock()
			return
		}
		min := c.batchSize
		if force {
			min = 1
		}
		if len(c.queue) < min || time.Now().Before(c.retryAt) {
			c.mu.Unlock()
			return
		}
		n := c.batchSize
		if n > len(c.queue) {
			n = len(c.queue)
		}
		batch := append([]apiclient.ServerEvent(nil), c.queue[:n]...)
		c.queue = c.queue[n:]
		c.inflight = true
		c.mu.Unlock()

		ok := c.sendBatch(ctx, batch)
		c.mu.Lock()
		c.inflight = false
		if ok {
			c.backoff = 0
			c.mu.Unlock()
			continue
		}
		c.enqueue(batch, true)
		if c.backoff == 0 {
			c.backoff = time.Second
		} else {
			c.backoff *= 2
			if c.backoff > retryMax {
				c.backoff = retryMax
			}
		}
		c.retryAt = time.Now().Add(c.backoff)
		c.mu.Unlock()
		return
	}
}

// Pending returns queued events waiting to be sent.
func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// Record queues one event. Never panics into the caller.
func (c *Client) Record(ev apiclient.ServerEvent) {
	defer func() {
		if r := recover(); r != nil {
			c.report(errors.New("kalathos: record panic"))
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if ev.Id == "" {
		ev.Id = "req_" + ULID(ev.Ts)
	}
	if ev.FieldId == "" {
		ev.FieldId = c.cfg.FieldID
	}
	c.enqueue([]apiclient.ServerEvent{ev}, false)
	if len(c.queue) >= c.batchSize && !c.inflight && !time.Now().Before(c.retryAt) {
		go c.drain(context.Background(), false)
	}
}

// Flush sends queued events now. Never returns an error to the caller.
func (c *Client) Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	c.retryAt = time.Time{}
	c.mu.Unlock()
	c.drain(ctx, true)
	return nil
}

// Close stops the timer and flushes.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	c.retryAt = time.Time{}
	c.mu.Unlock()
	return c.Flush(ctx)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Handler returns net/http middleware that records each request when the response finishes.
func (c *Client) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw, finish := c.track(w, r)
		if finish == nil {
			next.ServeHTTP(w, r)
			return
		}
		completed := false
		defer func() {
			if !completed {
				finish(false)
			}
		}()
		next.ServeHTTP(rw, r)
		completed = true
		finish(true)
	})
}

// track prepares recording r and returns the writer to pass on and the function that records
// it; a nil finish means r is not recorded. Panics in this code are reported, not raised; a
// panic in the host's handler is not recovered.
func (c *Client) track(w http.ResponseWriter, r *http.Request) (rw http.ResponseWriter, finish func(completed bool)) {
	defer func() {
		if p := recover(); p != nil {
			c.report(errors.New("kalathos: middleware panic"))
			rw, finish = w, nil
		}
	}()

	raw := r.URL.RequestURI()
	if raw == "" {
		raw = "/"
	}
	path, cleaned := CleanURL(raw, c.allowQuery)
	method := strings.ToUpper(r.Method)
	skipped := method == http.MethodOptions ||
		(c.skipStatic && IsStaticAsset(path)) ||
		(c.cfg.Skip != nil && c.cfg.Skip(r))
	if skipped {
		return w, nil
	}

	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 0}
	done := false
	finish = func(completed bool) {
		if done {
			return
		}
		done = true
		defer func() {
			if rec := recover(); rec != nil {
				c.report(errors.New("kalathos: finish panic"))
			}
		}()

		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		host := r.Host
		urlStr := cleaned
		if host != "" {
			urlStr = proto + "://" + host + cleaned
		}

		headers := PickHeaders(r.Header)
		reqEv := apiclient.ServerEventRequest{
			Method:     method,
			Url:        urlStr,
			DurationMs: time.Since(start).Milliseconds(),
			Headers:    &headers,
		}
		status := int64(rec.status)
		if completed && !rec.wrote {
			status = http.StatusOK // net/http answers a handler that wrote nothing with 200
		}
		if (completed || rec.wrote) && status > 0 {
			reqEv.Status = &status
		}
		if IsSigned(r.Header) {
			for k, v := range PickSignedHeaders(r.Header) {
				(*reqEv.Headers)[k] = v
			}
			var target string
			if c.cfg.GetTargetURI != nil {
				target = c.cfg.GetTargetURI(r)
			} else {
				target = TargetURI(r)
			}
			if target != "" {
				reqEv.TargetUri = &target
			}
		}
		if c.cfg.GetClientIP != nil {
			if ip := c.cfg.GetClientIP(r); ip != "" {
				reqEv.Ip = &ip
			}
		} else if ip := RemoteAddr(r); ip != "" {
			reqEv.Ip = &ip
		}
		if ua := r.UserAgent(); ua != "" {
			if len(ua) > 1024 {
				ua = ua[:1024]
			}
			reqEv.UserAgent = &ua
		}
		ev := apiclient.ServerEvent{
			Id:      "req_" + ULID(start.UnixMilli()),
			Ts:      start.UnixMilli(),
			FieldId: c.cfg.FieldID,
			Request: reqEv,
		}
		if c.cfg.GetSessionHint != nil {
			if hint := c.cfg.GetSessionHint(r); hint != "" {
				ev.SessionHint = &hint
			}
		}
		c.Record(ev)
	}

	return rec, finish
}

// Middleware wraps next using cfg. Events still queued when the process exits are lost; use
// MiddlewareWithClose (or New) to flush them on shutdown.
func Middleware(cfg Config) (func(http.Handler) http.Handler, error) {
	mw, _, err := MiddlewareWithClose(cfg)
	return mw, err
}

// MiddlewareWithClose is Middleware plus a close function that stops the background flush
// timer and sends every queued event, bounded by ctx. Call it on shutdown, after the HTTP
// server has stopped accepting requests; requests recorded afterwards are dropped. Closing
// twice is safe.
func MiddlewareWithClose(cfg Config) (func(http.Handler) http.Handler, func(context.Context) error, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return c.Handler, c.Close, nil
}

// NewAPIClient returns the OpenAPI-generated HTTP client for kalathos.ai.
func NewAPIClient(baseURL string, opts ...apiclient.ClientOption) (*apiclient.Client, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = DefaultEndpoint
	}
	return apiclient.NewClient(base+"/", opts...)
}
