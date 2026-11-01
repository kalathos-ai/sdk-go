# kalathos Go SDK

Server middleware for Go (`net/http`, Echo, Chi, etc.) that:

- reports each request to kalathos.ai (`POST /v1/ingest/server-events`), including Web Bot Auth signature forwarding;
- resolves **Fetcher Seeds** for crawlers and fetchers that never run JavaScript.

## Install

```bash
go get kalathos.ai/sdk-go@latest
```

```go
import kalathos "kalathos.ai/sdk-go"
```

## Request reporting

```go
client, err := kalathos.New(kalathos.Config{
    SecretKey: os.Getenv("KALATHOS_SECRET_KEY"), // kl_sk_… with ingest:write
    FieldID:   "fld_…",
    GetClientIP: kalathos.ForwardedForResolver(kalathos.IsPrivateAddress),
})
if err != nil { /* … */ }
defer client.Close(context.Background())

http.Handle("/", client.Handler(yourHandler))
```

The middleware never blocks or fails a request. Events are batched and retried in the background.

Events still queued when the process exits are lost, so close the reporter on shutdown. With the
plain middleware form, use `MiddlewareWithClose` (`Middleware` is kept for compatibility but has
no way to flush):

```go
mw, closeKalathos, err := kalathos.MiddlewareWithClose(kalathos.Config{SecretKey: …, FieldID: "fld_…"})
if err != nil { /* … */ }
srv := &http.Server{Addr: ":8080", Handler: mw(router)}
go srv.ListenAndServe()
<-ctx.Done()
_ = srv.Shutdown(shutdownCtx)     // stop accepting requests first
_ = closeKalathos(shutdownCtx)    // then flush queued events (bounded by the context)
```

## Fetcher Seeds

```go
seeds, err := kalathos.NewFetcherSeeds(kalathos.FetcherSeedsConfig{
    PublishableKey: os.Getenv("KALATHOS_PUBLISHABLE_KEY"),
    FieldID:        "fld_…",
    Origin:         "https://shop.example",
    Routes: []kalathos.SeedRoute{
        {Match: "/products/*", Goal: "purchase"},
    },
})
html := seeds.Render(r.Context(), r.URL.Path) // embed in HTML for every visitor
```

See the `@kalathos/node` README for the Honest Seed Policy and Web Bot Auth details; behavior matches the Node SDK.
