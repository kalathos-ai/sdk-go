package kalathos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"kalathos.ai/sdk-go/internal/apiclient"
)

// noSeedTTL is how long a 402 from the resolver is remembered at most.
const noSeedTTL = 5 * time.Minute

const (
	maxSeedHTML  = 8192
	maxSeedNodes = 400
	maxSeedDepth = 12
)

var seedTags = map[string]struct{}{
	"section": {}, "div": {}, "p": {}, "h1": {}, "h2": {}, "h3": {}, "h4": {}, "h5": {}, "h6": {},
	"ul": {}, "ol": {}, "li": {}, "a": {}, "strong": {}, "em": {}, "b": {}, "i": {}, "span": {}, "br": {},
	"small": {}, "dl": {}, "dt": {}, "dd": {},
}

var seedAttrs = map[string]struct{}{
	"id": {}, "class": {}, "aria-labelledby": {}, "aria-label": {},
}

// SeedRoute maps paths to goals for Fetcher Seeds.
type SeedRoute struct {
	Match     any // string prefix/*, *regexp.Regexp, or func(string) bool
	Goal      string
	Archetype string
}

// FetcherSeedsConfig configures server-side seed rendering.
type FetcherSeedsConfig struct {
	// SecretKey is a secret key with seeds:read (kl_sk_live_… or kl_sk_test_…); keep it on the
	// server. Fetcher Seeds need the Scale plan. Publishable keys only resolve seeds in the
	// browser (kalathos.js).
	SecretKey string
	// FieldID is the field (site) the pages belong to; a field-bound key must belong to it.
	FieldID  string
	Endpoint string

	Routes         []SeedRoute
	TimeoutMs      int
	FetchTimeoutMs int
	CacheTTLMs     int
	StaleTTLMs     int
	MaxEntries     int
	OnError        func(error)

	HTTPClient *http.Client
}

// ResolvedSeed is a validated seed ready to embed.
type ResolvedSeed struct {
	SeedID  string
	Variant string
	HTML    string
}

// FetcherSeeds resolves and renders honest server-side seeds.
type FetcherSeeds struct {
	cfg    FetcherSeedsConfig
	api    *apiclient.Client
	routes []SeedRoute

	timeout      time.Duration
	fetchTimeout time.Duration
	ttl          time.Duration
	staleTTL     time.Duration
	maxEntries   int
	failureTTL   time.Duration

	mu       sync.Mutex
	cache    map[string]seedEntry
	inflight map[string]*seedInflight
}

type seedEntry struct {
	seed       *ResolvedSeed
	freshUntil time.Time
	staleUntil time.Time
	usedAt     time.Time
}

type seedInflight struct {
	done  chan struct{}
	entry seedEntry
	err   error
}

// PathArchetype derives an archetype from a path like kalathos.js.
func PathArchetype(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s == "" || strings.ContainsAny(s, "0123456789") || len(s) > 24 {
			continue
		}
		segs = append(segs, s)
	}
	out := strings.Join(segs, "_")
	out = regexp.MustCompile(`[^\w-]`).ReplaceAllString(out, "")
	if out == "" {
		out = "home"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// NewFetcherSeeds builds a Fetcher Seeds helper.
func NewFetcherSeeds(cfg FetcherSeedsConfig) (*FetcherSeeds, error) {
	if !strings.HasPrefix(cfg.SecretKey, "kl_sk_live_") && !strings.HasPrefix(cfg.SecretKey, "kl_sk_test_") {
		return nil, errors.New("kalathos: SecretKey must start with kl_sk_live_ or kl_sk_test_; publishable keys only resolve seeds in the browser")
	}
	if cfg.FieldID == "" {
		return nil, errors.New("kalathos: FieldID is required")
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	if base == "" {
		base = EndpointForKey(cfg.SecretKey)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	api, err := apiclient.NewClient(base+"/", apiclient.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	timeoutMs := cfg.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 800
	}
	fetchMs := cfg.FetchTimeoutMs
	if fetchMs < timeoutMs {
		fetchMs = 5000
	}
	ttlMs := cfg.CacheTTLMs
	if ttlMs <= 0 {
		ttlMs = 300_000
	}
	staleMs := cfg.StaleTTLMs
	if staleMs <= 0 {
		staleMs = 3_600_000
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 500
	}
	ttl := time.Duration(ttlMs) * time.Millisecond
	return &FetcherSeeds{
		cfg:          cfg,
		api:          api,
		routes:       cfg.Routes,
		timeout:      time.Duration(timeoutMs) * time.Millisecond,
		fetchTimeout: time.Duration(fetchMs) * time.Millisecond,
		ttl:          ttl,
		staleTTL:     time.Duration(staleMs) * time.Millisecond,
		maxEntries:   maxEntries,
		failureTTL:   minDuration(ttl, 30*time.Second),
		cache:        make(map[string]seedEntry),
		inflight:     make(map[string]*seedInflight),
	}, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (f *FetcherSeeds) report(err error) {
	if err == nil || f.cfg.OnError == nil {
		return
	}
	defer func() { _ = recover() }()
	f.cfg.OnError(err)
}

func routeMatches(match any, path string) bool {
	switch m := match.(type) {
	case func(string) bool:
		return m(path)
	case *regexp.Regexp:
		return m.MatchString(path)
	case string:
		if strings.HasSuffix(m, "*") {
			return strings.HasPrefix(path, m[:len(m)-1])
		}
		return path == m
	default:
		return false
	}
}

func (f *FetcherSeeds) route(path string) *SeedRoute {
	for i := range f.routes {
		r := &f.routes[i]
		if routeMatches(r.Match, path) {
			return r
		}
	}
	return nil
}

// Resolve returns the seed for a goal and archetype, or nil.
func (f *FetcherSeeds) Resolve(ctx context.Context, goal, archetype string) (*ResolvedSeed, error) {
	key := goal + "\x00" + archetype
	now := time.Now()
	f.mu.Lock()
	if hit, ok := f.cache[key]; ok && now.Before(hit.freshUntil) {
		hit.usedAt = now
		f.cache[key] = hit
		seed := hit.seed
		f.mu.Unlock()
		return seed, nil
	}
	var stale *ResolvedSeed
	if hit, ok := f.cache[key]; ok && now.Before(hit.staleUntil) {
		stale = hit.seed
	}
	pending := f.startRefreshLocked(key, goal, archetype)
	f.mu.Unlock()

	if stale != nil {
		go func() { _, _ = pending.wait(context.WithoutCancel(ctx)) }() //nolint:gosec // background cache refresh
		return stale, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	entry, err := pending.wait(waitCtx)
	if err != nil || entry.seed == nil {
		return nil, nil
	}
	return entry.seed, nil
}

// startRefreshLocked starts (or joins) the refresh of key; the caller holds f.mu.
func (f *FetcherSeeds) startRefreshLocked(key, goal, archetype string) *seedInflight {
	if p, ok := f.inflight[key]; ok {
		return p
	}
	p := &seedInflight{done: make(chan struct{})}
	f.inflight[key] = p
	go func() {
		entry, err := f.fetchSeed(context.Background(), goal, archetype)
		p.entry = entry
		p.err = err
		f.mu.Lock()
		if err == nil {
			f.storeLocked(key, entry)
		} else {
			f.report(err)
			now := time.Now()
			if prev, ok := f.cache[key]; ok && now.Before(prev.staleUntil) {
				prev.freshUntil = now.Add(f.failureTTL)
				prev.usedAt = now
				f.storeLocked(key, prev)
				p.entry = prev
			} else {
				e := seedEntry{seed: nil, freshUntil: now.Add(f.failureTTL), staleUntil: now.Add(f.failureTTL), usedAt: now}
				f.storeLocked(key, e)
				p.entry = e
			}
		}
		delete(f.inflight, key)
		f.mu.Unlock()
		close(p.done)
	}()
	return p
}

func (p *seedInflight) wait(ctx context.Context) (seedEntry, error) {
	select {
	case <-ctx.Done():
		return seedEntry{}, ctx.Err()
	case <-p.done:
		return p.entry, p.err
	}
}

func (f *FetcherSeeds) storeLocked(key string, e seedEntry) {
	if _, ok := f.cache[key]; !ok && len(f.cache) >= f.maxEntries {
		var oldestKey string
		oldest := time.Time{}
		for k, v := range f.cache {
			if oldest.IsZero() || v.usedAt.Before(oldest) {
				oldest = v.usedAt
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(f.cache, oldestKey)
		}
	}
	f.cache[key] = e
}

func (f *FetcherSeeds) fetchSeed(ctx context.Context, goal, archetype string) (seedEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, f.fetchTimeout)
	defer cancel()
	params := apiclient.SeedsServerResolveParams{
		Field:     f.cfg.FieldID,
		Goal:      &goal,
		Archetype: &archetype,
	}
	res, err := f.api.SeedsServerResolve(ctx, &params, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+f.cfg.SecretKey)
		return nil
	})
	if err != nil {
		return seedEntry{}, err
	}
	defer res.Body.Close()
	maxAge := parseMaxAge(res.Header.Get("Cache-Control"))
	// 204 and 402 both mean "no seed": 402 is remembered for a few minutes, not retried on every render.
	if res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusPaymentRequired {
		_, _ = io.Copy(io.Discard, res.Body)
		life := f.ttl
		if maxAge >= 0 {
			life = minDuration(f.ttl, time.Duration(maxAge)*time.Second)
		} else if res.StatusCode == http.StatusPaymentRequired {
			life = minDuration(f.ttl, noSeedTTL)
		}
		now := time.Now()
		return seedEntry{seed: nil, freshUntil: now.Add(life), staleUntil: now.Add(life + f.staleTTL), usedAt: now}, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return seedEntry{}, fmt.Errorf("kalathos: seeds/server-resolve responded %s", res.Status)
	}
	var body apiclient.ResolvedSeed
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return seedEntry{}, err
	}
	html := RenderSeedHTML(body.Html, body.Jsonld, body.SeedId, body.Variant)
	if html == "" {
		return seedEntry{}, fmt.Errorf("kalathos: seed %s failed validation and is not rendered", body.SeedId)
	}
	seed := &ResolvedSeed{SeedID: body.SeedId, Variant: body.Variant, HTML: html}
	life := f.ttl
	if maxAge >= 0 {
		life = minDuration(f.ttl, time.Duration(maxAge)*time.Second)
	}
	now := time.Now()
	return seedEntry{seed: seed, freshUntil: now.Add(life), staleUntil: now.Add(life + f.staleTTL), usedAt: now}, nil
}

func parseMaxAge(cc string) int {
	m := regexp.MustCompile(`max-age=(\d+)`).FindStringSubmatch(cc)
	if len(m) < 2 {
		return -1
	}
	var n int
	_, _ = fmt.Sscanf(m[1], "%d", &n)
	return n
}

// Render returns seed HTML for a path, or "".
func (f *FetcherSeeds) Render(ctx context.Context, path string) string {
	defer func() {
		if r := recover(); r != nil {
			f.report(errors.New("kalathos: render panic"))
		}
	}()
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	r := f.route(path)
	if r == nil {
		return ""
	}
	arch := r.Archetype
	if arch == "" {
		arch = PathArchetype(path)
	}
	seed, _ := f.Resolve(ctx, r.Goal, arch)
	if seed == nil {
		return ""
	}
	return seed.HTML
}

// Middleware sets kalathos seed HTML on the request context for matching GET/HEAD pages.
func (f *FetcherSeeds) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.ToUpper(r.Method)
		if method != http.MethodGet && method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		if r.RequestURI != "" {
			if i := strings.IndexAny(r.RequestURI, "?#"); i >= 0 {
				path = r.RequestURI[:i]
			} else {
				path = r.RequestURI
			}
		}
		html := f.Render(r.Context(), path)
		ctx := context.WithValue(r.Context(), seedContextKey{}, html)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type seedContextKey struct{}

// SeedFromContext returns seed HTML stored by FetcherSeeds middleware.
func SeedFromContext(ctx context.Context) string {
	v, _ := ctx.Value(seedContextKey{}).(string)
	return v
}

var seedTagRe = regexp.MustCompile(`^<(\/?)([a-zA-Z][a-zA-Z0-9]*)((?:\s+[a-zA-Z][a-zA-Z0-9-]*="[^"<>]*")*)\s*(\/?)>`)
var seedAttrRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9-]*)="([^"<>]*)"`)

// RenderSeedHTML validates seed markup and adds identity attributes and JSON-LD.
func RenderSeedHTML(html string, jsonld map[string]interface{}, seedID, variant string) string {
	if len(html) > maxSeedHTML || !regexp.MustCompile(`^sd_[A-Za-z0-9_-]+$`).MatchString(seedID) {
		return ""
	}
	var out []string
	var stack []string
	nodes := 0
	text := false
	rootOpen := -1
	roots := 0
	for i := 0; i < len(html); {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			chunk := html[i:]
			if len(stack) == 0 && strings.TrimSpace(chunk) != "" {
				return ""
			}
			if strings.TrimSpace(chunk) != "" {
				text = true
			}
			out = append(out, chunk)
			break
		}
		lt += i
		chunk := html[i:lt]
		if chunk != "" {
			if len(stack) == 0 && strings.TrimSpace(chunk) != "" {
				return ""
			}
			if strings.TrimSpace(chunk) != "" {
				text = true
			}
			out = append(out, chunk)
		}
		m := seedTagRe.FindStringSubmatch(html[lt:])
		if m == nil {
			return ""
		}
		whole, close, rawTag, attrs, selfClose := m[0], m[1], m[2], m[3], m[4]
		tag := strings.ToLower(rawTag)
		if _, ok := seedTags[tag]; !ok {
			return ""
		}
		if close != "" {
			if len(stack) == 0 || stack[len(stack)-1] != tag || attrs != "" {
				return ""
			}
			stack = stack[:len(stack)-1]
			out = append(out, "</"+tag+">")
		} else {
			nodes++
			if nodes > maxSeedNodes {
				return ""
			}
			var kept []string
			for _, a := range seedAttrRe.FindAllStringSubmatch(attrs, -1) {
				name := strings.ToLower(a[1])
				value := a[2]
				switch {
				case seedAttrAllowed(name):
					if len(value) > 256 {
						value = value[:256]
					}
					kept = append(kept, fmt.Sprintf(`%s="%s"`, name, value))
				case name == "href" && tag == "a":
					if safeHref(value) {
						kept = append(kept, fmt.Sprintf(`href="%s"`, value))
					} else {
						return ""
					}
				case name == "data-kalathos-seed" && tag == "section":
					if regexp.MustCompile(`^[a-z]{1,32}$`).MatchString(value) {
						kept = append(kept, fmt.Sprintf(`data-kalathos-seed="%s"`, value))
					} else {
						return ""
					}
				default:
					return ""
				}
			}
			if tag == "a" {
				kept = append(kept, `rel="noopener"`)
			}
			if len(stack) == 0 {
				if tag != "section" {
					return ""
				}
				roots++
				if roots > 1 {
					return ""
				}
				hasData := false
				for _, k := range kept {
					if strings.HasPrefix(k, `data-kalathos-seed=`) {
						hasData = true
						break
					}
				}
				if !hasData {
					kept = append([]string{`data-kalathos-seed="harvest"`}, kept...)
				}
				rootOpen = len(out)
			}
			open := "<" + tag
			if len(kept) > 0 {
				open += " " + strings.Join(kept, " ")
			}
			open += ">"
			out = append(out, open)
			if tag != "br" {
				if selfClose != "" {
					return ""
				}
				stack = append(stack, tag)
				if len(stack) > maxSeedDepth {
					return ""
				}
			}
		}
		i = lt + len(whole)
	}
	if len(stack) != 0 || rootOpen < 0 || !text {
		return ""
	}
	ids := fmt.Sprintf(` data-seed-id="%s" data-seed-source="server"`, seedID)
	if variant != "" {
		v := variant
		if len(v) > 64 {
			v = v[:64]
		}
		ids += ` data-seed-variant="` + escAttr(v) + `"`
	}
	out[rootOpen] = strings.TrimSuffix(out[rootOpen], ">") + ids + ">"
	ld := ldScript(jsonld)
	if ld != "" {
		for i := len(out) - 1; i >= 0; i-- {
			if out[i] == "</section>" {
				out = append(out[:i], append([]string{ld}, out[i:]...)...)
				break
			}
		}
	}
	return strings.Join(out, "")
}

func seedAttrAllowed(name string) bool {
	_, ok := seedAttrs[name]
	return ok
}

func escAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

func safeHref(raw string) bool {
	v := strings.ReplaceAll(raw, "&amp;", "&")
	v = strings.TrimSpace(v)
	if regexp.MustCompile(`[\s\\]|&#|&[a-z]+;`).MatchString(v) {
		return false
	}
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return true
	}
	if v == "/" {
		return true
	}
	return regexp.MustCompile(`^https?://[a-z0-9.-]+(:\d+)?(/|$|\?|#)`).MatchString(v)
}

func ldScript(jsonld map[string]interface{}) string {
	if len(jsonld) == 0 {
		return ""
	}
	b, err := json.Marshal(jsonld)
	if err != nil || len(b) == 0 || string(b) == "{}" {
		return ""
	}
	text := strings.ReplaceAll(string(b), "<", "\\u003c")
	return `<script type="application/ld+json">` + text + `</script>`
}
