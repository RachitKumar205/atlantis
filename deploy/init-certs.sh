#!/bin/sh
# init-certs.sh — first-boot TLS bootstrap for the atlantis self-host bundle.
#
# Writes to CERT_DIR (default /certs):
#   ca.crt                    — self-signed root CA (public; shared with all services)
#   server.crt / server.key   — atlantis gRPC server cert
#                               SANs: DNS:atlantis, DNS:localhost, IP:127.0.0.1
#                               plus DNS:<ATLANTIS_DOMAIN> if set
#   console.crt / console.key — mTLS client cert for atlantis-console (CN=atlantis-console)
#
# Writes to CA_PRIVATE_DIR (default /ca-private):
#   ca.key   — CA private key (never written to CERT_DIR; only the signer mounts this)
#   ca.crt   — copy so the signer can load the full CA bundle without mounting atl-certs
#
# Incremental, per artifact. A file that is present, unexpired and current is
# left exactly as it is; only what is missing, expired or stale is written. So
# re-running is safe, and deleting one file reissues that file alone.
#
# The exception is the CA: regenerating it invalidates every certificate it
# signed, so a missing or expired CA reissues both leaves with it. Caller certs
# are NOT reissued and must be requested again after a CA change —
# `make dev-caller-cert` locally, the signer service in a deployed stack.
#
# To bring your own CA: pre-populate CERT_DIR and CA_PRIVATE_DIR with the
# files listed above; this script will not overwrite them.

set -e

CERT_DIR="${CERT_DIR:-/certs}"
CA_PRIVATE_DIR="${CA_PRIVATE_DIR:-/ca-private}"
ATLANTIS_DOMAIN="${ATLANTIS_DOMAIN:-}"
# 10-year CA + leaf. There is no online rotation path today, so a stack runs
# against this CA for its life. To rotate: stop the stack, delete the files in
# CERT_DIR and CA_PRIVATE_DIR, re-run this script, then re-issue every caller
# certificate.
DAYS=3650

mkdir -p "$CERT_DIR" "$CA_PRIVATE_DIR"

# The SAN the server certificate must carry. Computed here because the
# staleness check below compares against it, not only the issuing step.
SAN="DNS:atlantis,DNS:localhost,IP:127.0.0.1"
if [ -n "$ATLANTIS_DOMAIN" ]; then
    SAN="${SAN},DNS:${ATLANTIS_DOMAIN}"
fi

# ── What needs generating ─────────────────────────────────────────────────────
#
# Decided per artifact rather than all-or-nothing. The previous version skipped
# only when all six files were present, which made every partial state
# unrecoverable in both directions: five files present and the script
# regenerated the CA, orphaning callers already holding certs signed by the old
# one; six files present and the script skipped, even when the server cert no
# longer covered ATLANTIS_DOMAIN.
#
# The CA is the root of the ordering. A regenerated CA invalidates every leaf it
# signed, so it forces both leaves to be reissued — a leaf kept across a CA
# change verifies against nothing.

need_ca=0
need_server=0
need_console=0

[ -f "$CERT_DIR/ca.crt" ] && [ -f "$CA_PRIVATE_DIR/ca.key" ] || need_ca=1

if [ "$need_ca" = 0 ]; then
    # Present but expired is worse than absent: the file passes an existence
    # check forever while every handshake against it fails.
    openssl x509 -in "$CERT_DIR/ca.crt" -noout -checkend 0 >/dev/null 2>&1 || {
        echo "[certs] CA has expired — regenerating it and both leaf certificates"
        need_ca=1
    }
fi

[ -f "$CERT_DIR/server.crt" ] && [ -f "$CERT_DIR/server.key" ] || need_server=1
[ -f "$CERT_DIR/console.crt" ] && [ -f "$CERT_DIR/console.key" ] || need_console=1

if [ "$need_server" = 0 ]; then
    openssl x509 -in "$CERT_DIR/server.crt" -noout -checkend 0 >/dev/null 2>&1 || {
        echo "[certs] server certificate has expired — reissuing"
        need_server=1
    }
fi
if [ "$need_console" = 0 ]; then
    openssl x509 -in "$CERT_DIR/console.crt" -noout -checkend 0 >/dev/null 2>&1 || {
        echo "[certs] console certificate has expired — reissuing"
        need_console=1
    }
fi

# ATLANTIS_DOMAIN changed since the server cert was issued.
#
# Without this the operator sets a domain, restarts, and every client dialling
# it fails hostname verification against a certificate that predates the
# setting — with no message anywhere saying the cert is the reason.
#
# Asked as "is the wanted name absent", not "does the SAN match exactly": a cert
# covering MORE names than requested still works, and rotating on that would
# reissue on every run after the domain is unset.
#
# One SAN entry per line, then an exact whole-line match. Both matter. A
# substring search would accept a cert carrying DNS:example.com.attacker.test
# as covering example.com, and an unanchored pattern would read the dots in the
# domain as regex wildcards. An earlier version of this check used neither and
# also put `$` inside a \(...\) group, where it is a literal dollar sign rather
# than an anchor — so it never matched, and every run reissued the server cert.
if [ "$need_server" = 0 ] && [ -n "$ATLANTIS_DOMAIN" ]; then
    if ! openssl x509 -in "$CERT_DIR/server.crt" -noout -ext subjectAltName 2>/dev/null \
         | tr ',' '\n' \
         | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' \
         | grep -qxF "DNS:${ATLANTIS_DOMAIN}"; then
        echo "[certs] server certificate does not cover ATLANTIS_DOMAIN=${ATLANTIS_DOMAIN} — reissuing"
        need_server=1
    fi
fi

if [ "$need_ca" = 1 ]; then
    need_server=1
    need_console=1
fi

if [ "$need_ca" = 0 ] && [ "$need_server" = 0 ] && [ "$need_console" = 0 ]; then
    echo "[certs] all certificates present, unexpired and current — skipping generation"
    exit 0
fi

# ── CA ────────────────────────────────────────────────────────────────────────
# The CA private key is written ONLY to CA_PRIVATE_DIR (the atl-ca-private
# volume).  It is never written to CERT_DIR (atl-certs), so the atlantis
# server and console containers cannot access it. Only the signer mounts
# atl-ca-private.
if [ "$need_ca" = 1 ]; then
    echo "[certs] generating local CA..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CA_PRIVATE_DIR/ca.key"
    openssl req -new -x509 \
        -key "$CA_PRIVATE_DIR/ca.key" \
        -out "$CERT_DIR/ca.crt" \
        -days "$DAYS" \
        -subj "/CN=atlantis-self-host-ca"

    # Copy ca.crt into CA_PRIVATE_DIR so the signer can read the full CA bundle
    # without needing to mount atl-certs.
    cp "$CERT_DIR/ca.crt" "$CA_PRIVATE_DIR/ca.crt"
fi

# ── Server cert ───────────────────────────────────────────────────────────────
if [ "$need_server" = 1 ]; then
    echo "[certs] generating server certificate (SAN: ${SAN})..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/server.key"
    openssl req -new \
        -key "$CERT_DIR/server.key" \
        -out /tmp/atl-server.csr \
        -subj "/CN=atlantis"

    printf "subjectAltName=%s\n" "$SAN" > /tmp/atl-server-san.ext

    openssl x509 -req \
        -in /tmp/atl-server.csr \
        -CA "$CERT_DIR/ca.crt" \
        -CAkey "$CA_PRIVATE_DIR/ca.key" \
        -CAcreateserial \
        -out "$CERT_DIR/server.crt" \
        -days "$DAYS" \
        -extfile /tmp/atl-server-san.ext

    rm /tmp/atl-server.csr /tmp/atl-server-san.ext
fi

# ── Console client cert ───────────────────────────────────────────────────────
# CN=atlantis-console is used by the server's caller-identity check and
# the per-CN rate-limit override (RATE_LIMIT_PER_CALLER).
if [ "$need_console" = 1 ]; then
    echo "[certs] generating console client certificate (CN=atlantis-console)..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/console.key"
    openssl req -new \
        -key "$CERT_DIR/console.key" \
        -out /tmp/atl-console.csr \
        -subj "/CN=atlantis-console"

    openssl x509 -req \
        -in /tmp/atl-console.csr \
        -CA "$CERT_DIR/ca.crt" \
        -CAkey "$CA_PRIVATE_DIR/ca.key" \
        -CAcreateserial \
        -out "$CERT_DIR/console.crt" \
        -days "$DAYS"

    rm /tmp/atl-console.csr
fi

# ── Permissions ───────────────────────────────────────────────────────────────
# 644 so non-root service containers (atlantis UID 10001, console app user,
# signer's "signer" user) can read keys from the volumes. The security
# boundary here is the *volume mount scope*, not the file mode:
#   - atl-certs is mounted by atlantis (ro) + console (ro) + certs (rw)
#   - atl-ca-private is mounted ONLY by signer (ro) + certs (rw)
# Nothing else in the compose stack ever sees the CA key.
chmod 644 "$CERT_DIR/"*.key "$CERT_DIR/"*.crt
chmod 644 "$CA_PRIVATE_DIR/ca.key" "$CA_PRIVATE_DIR/ca.crt"
# (Caller-side keys get 600 because they live on a host filesystem, not inside
# a scoped volume.)

echo "[certs] generated:"
ls -la "$CERT_DIR/"
echo "[certs] CA private dir:"
ls -la "$CA_PRIVATE_DIR/"
echo "[certs] done."
