# Changelog

## 0.2.0

- **Breaking:** `FetcherSeedsConfig` takes `SecretKey` (a `kl_sk_…` key with `seeds:read`) instead of
  `PublishableKey`, and the `Origin` field is removed. `NewFetcherSeeds` returns an error for any other
  key. The lookup calls `GET /v1/seeds/server-resolve` with `Authorization: Bearer <secret key>`.
- The middleware is unchanged.
- Fetcher Seeds need the Scale or Enterprise plan; below Scale the resolver answers 402 and nothing is rendered, as before.

## 0.1.0

- First release.
