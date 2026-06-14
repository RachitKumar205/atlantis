# Reverse-proxy examples (trusted front-proxy mode)

Starting-point configs for running atlantis behind a TLS-terminating reverse
proxy while preserving per-caller mTLS identity. The proxy terminates the
client's mTLS, validates the client cert against the atlantis CA, and forwards
the verified cert to the gRPC backend in `X-Forwarded-Client-Cert` (encoding
differs per proxy — see the table); the server re-validates it and derives
caller identity from it. Every identity and authz decision stays at the origin.

| File | Proxy |
|---|---|
| `nginx.conf` | nginx (`grpc_pass` + `ssl_verify_client`) |
| `Caddyfile` | Caddy (`client_auth` + base64-DER header) |
| `envoy.yaml` | Envoy (native XFCC) |

## What each config assumes

- The server runs with mTLS **and** `ATL_TRUSTED_PROXY_CALLERS=atlantis-proxy`.
- Cert files mounted at `/certs`:
  - `edge.crt` / `edge.key` — the public-facing edge cert the proxy presents to clients.
  - `ca.crt` — the atlantis CA, used to verify incoming client certs **and** the backend server cert.
  - `proxy.crt` / `proxy.key` — the proxy's own client cert, CN `atlantis-proxy`, used to re-originate mTLS to the backend.
- The backend reachable at `atlantis:9090` (the default `GRPC_LISTEN`).

Mint the proxy cert like any caller cert, with CN `atlantis-proxy`. See
[docs/guides/run-behind-a-reverse-proxy.md](../../docs/guides/run-behind-a-reverse-proxy.md)
for the end-to-end setup, the security model, and verification steps.

These are starting points — verify directive names against your proxy version
before production use.
