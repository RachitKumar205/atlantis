# Run behind a reverse proxy

After this recipe you'll have atlantis behind a TLS-terminating reverse proxy (nginx, Caddy, or Envoy) that handles L7 ingress — routing, observability, WAF — while every caller still authenticates by its client certificate.

Prereqs:

- atlantis running with mTLS (the three `TLS_*` files set); see [Deploy to production](deploy-to-production.md).
- A reverse proxy you control that can terminate TLS and re-originate it to the backend.

## When to use this

atlantis authenticates each caller by its client-certificate CN and binds that CN to a registered cert fingerprint. Both checks need the client cert *at the server*, so by default the TLS session has to terminate at atlantis — which is why the simplest production setup is L4 / SNI passthrough (the proxy forwards raw TLS without terminating).

**Passthrough is the recommended default.** It adds no trust surface: the client cert is verified end-to-end at the origin. Reach for trusted front-proxy mode below only when the proxy must terminate TLS — to put atlantis behind a shared L7 ingress, a WAF, or path-based routing.

## How it works

The proxy terminates the client's mTLS, validates the client cert against the atlantis CA, and forwards the verified cert to the backend in a header (`X-Forwarded-Client-Cert`). It re-originates mTLS to the backend as a dedicated **proxy identity**. The server, seeing a connection from a configured trusted proxy, **re-validates** the forwarded cert against its own CA and derives the caller identity (and the cert-binding fingerprint) from it.

The proxy delegates only the TLS transport. The origin re-validates the forwarded cert — chain to the CA, `clientAuth` key usage, and the validity window — so identity and authz decisions never leave the server. A client that connects directly and forges the header is ignored, because its own peer CN isn't a trusted proxy.

## 1. Mint a proxy client cert

The proxy needs its own client cert to authenticate to the backend. Mint it like any caller cert, with a CN that will be your trusted-proxy identity — `atlantis-proxy`:

```
make self-host-caller-cert CALLER=atlantis-proxy
```

Certs from the self-host signer already carry `clientAuth` key usage, which is what the origin's re-validation checks for. If you mint the proxy cert from your own PKI instead, give it `clientAuth` (or no EKU at all). The CN must not collide with a real caller — `atlantis-proxy` is dedicated to the proxy.

## 2. Configure the server

Set the trusted-proxy CN (and, optionally, the policy knobs):

```
ATL_TRUSTED_PROXY_CALLERS=atlantis-proxy
# Optional — defaults shown:
# ATL_TRUSTED_PROXY_CERT_HEADER=x-forwarded-client-cert
# ATL_TRUSTED_PROXY_MAY_APPLY=true      # a forwarded caller may run its own plan/apply
# ATL_TRUSTED_PROXY_MAY_OPERATE=false   # cross-caller operator RPCs stay on direct mTLS
```

The mode is inert until `ATL_TRUSTED_PROXY_CALLERS` is set, and it requires mTLS (the server refuses to start otherwise — the proxy authenticates as an mTLS client). A CN in this set is *only* ever a proxy: a connection from it must forward a valid client cert, or its requests are rejected.

## 3. Configure the proxy

Use the starting-point configs in [`deploy/reverse-proxy/`](../../deploy/reverse-proxy/): `nginx.conf`, `Caddyfile`, or `envoy.yaml`. Each one:

- terminates client mTLS and verifies the client cert against the atlantis CA,
- forwards the verified cert in `X-Forwarded-Client-Cert` (URL-encoded PEM for nginx, base64-DER for Caddy, XFCC for Envoy) and **replaces** any client-supplied copy of that header,
- re-originates mTLS to `atlantis:9090` using the `atlantis-proxy` cert.

Verify the directive names against your proxy version before production use.

## 4. Verify

- Run `tide plan` / `tide apply` and a normal entity RPC through the proxy → they succeed as the real caller.
- From a direct (non-proxy) client, send a forged `X-Forwarded-Client-Cert` → you still authenticate as your own cert CN, not the forged one.
- Attempt an operator RPC (`tidectl` register/revoke/adopt) through the proxy → refused; over direct mTLS → succeeds. See [Admin-plane split](#admin-plane-split).
- Direct mTLS callers (no proxy) keep working unchanged.

## Admin-plane split

A forwarded identity can carry a caller's own data plane and its own `tide plan` / `apply` (`ATL_TRUSTED_PROXY_MAY_APPLY=true`, the default) — the edge can already act as that caller for its own data, so self-scoped schema changes are the same trust domain. **Cross-caller operator RPCs** — register/revoke other callers, adopt, rollback, set aliases — stay off the edge by default (`ATL_TRUSTED_PROXY_MAY_OPERATE=false`): those manage the trust system itself, so they require direct mTLS unless you explicitly opt in. Enabling `ATL_TRUSTED_PROXY_MAY_OPERATE` makes the proxy able to mint operator identity, so grant it only when the edge is as trusted as the server.

## Security notes

- **The origin stays the cryptographic authority.** The forwarded cert is re-validated (chain + `clientAuth` usage + validity) against the server's own CA. The proxy's validation is a convenience, not the boundary — a sloppy or misconfigured proxy can't smuggle an unverified identity past the origin.
- **Cert binding survives the proxy.** The forwarded cert's fingerprint must still match the caller's registered fingerprint, so per-caller binding and revocation work exactly as they do for direct mTLS.
- **Trusting the proxy is a real grant.** A CN in `ATL_TRUSTED_PROXY_CALLERS` can assert any caller identity (gated by the admin-plane split). A compromised proxy cert is therefore equivalent to a high-privilege credential — keep it short-lived and rotate it like any caller cert.
- **Single CA chain.** These examples assume the leaf is issued directly off the atlantis CA (the self-host default). Forwarding a multi-intermediate chain is not yet handled — the re-validation fails closed if an intermediate is missing.
