#!/bin/sh
# init-certs.sh — first-boot TLS bootstrap for the atlantis self-host bundle.
#
# Writes to CERT_DIR (default /certs):
#   ca.crt                    — self-signed root CA (public; shared with all services)
#   server.crt / server.key   — atlantis gRPC server cert
#                               SANs: DNS:atlantis, DNS:localhost, IP:127.0.0.1
#                               plus DNS:<ATLANTIS_DOMAIN> if set
#   console.crt / console.key — mTLS client cert for atlantis-console (CN=atlantis-console)
#   signer-ca.crt             — the authority the signer accepts CALLERS BY (see below)
#   signer-client.crt / .key  — the console's credential to the signer (CN=atlantis-console)
#   enroll-server.crt / .key  — the console's enrolment listener; chains to ca.crt
#
# Writes to CA_PRIVATE_DIR (default /ca-private):
#   ca.key   — CA private key (never written to CERT_DIR; only the signer mounts this)
#   ca.crt   — copy so the signer can load the full CA bundle without mounting atl-certs
#   signer-ca.key / signer-ca.crt   — the signer's client authority
#   signer-server.crt / .key        — the signer's own TLS identity
#
# ── Two authorities, and why ─────────────────────────────────────────────────
#
# ca.* is what the signer ISSUES caller certificates from. signer-ca.* is what
# the signer ACCEPTS callers by. They must not be the same authority: every
# caller certificate is marked for client authentication, so a signer trusting
# its own issuing CA would accept every certificate it had ever produced as a
# credential to itself — and one caller could then obtain another's identity.
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
# 10-year CA. There is no online rotation path today, so a stack runs against
# this CA for its life. To rotate: stop the stack, delete the files in CERT_DIR
# and CA_PRIVATE_DIR, re-run this script, then re-issue every caller
# certificate.
#
# Client certificates take the same lifetime. Nothing caps how long a client
# certificate may live — the limit below is a rule about TLS *server*
# certificates — and rotating them means re-issuing to every holder.
#
# This deliberately no longer matches internal/cloud/provision/certs, where the
# console's client certificates last thirty days. The difference is not drift:
# there, the provisioner reissues them on its reconcile pass, and a short life is
# safe precisely because something renews it. Nothing renews what this script
# writes — a stack built from these files runs against them until somebody
# re-runs it by hand — so shortening the number here would schedule the outage
# the paragraph above is warning about.
DAYS=3650

# TLS server certificates: 820 days.
#
# Apple's verifier refuses a server certificate valid for more than 825 days,
# and Go defers to it whenever it falls back to the system roots. The failure is
# not "untrusted", it is `x509: "name" certificate is not standards compliant` —
# a message naming one of Apple's rules that says nothing about trust, and which
# cost an hour of misdiagnosis the first time `tide login` met it.
#
# Measured at the boundary rather than looked up: 825 days fails with
# `certificate signed by unknown authority` (the honest error), 826 days with
# the opaque one.
#
# 820 rather than 825 leaves room for clock skew between issuing and verifying.
SERVER_DAYS=820

# How close to expiry a certificate may get before this script reissues it.
#
# `-checkend 0` means "already expired", which at a ten-year lifetime is
# theoretical and at 820 days is a scheduled outage: the certs step runs once at
# container start, so a stack whose leaf lapsed stays broken until somebody
# restarts it. Thirty days is a window a weekly restart cannot miss.
#
# The CAs keep `-checkend 0`. Reissuing one invalidates every certificate under
# it, so that is an event for a person, not for a start-up script.
RENEW_WINDOW=$((30 * 86400))

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
# The signer's own trust domain. Deliberately a SECOND authority — see the
# generation block below for why it must not be the one above.
need_signer_ca=0
need_signer_server=0
need_signer_client=0
need_enroll=0

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
    openssl x509 -in "$CERT_DIR/server.crt" -noout -checkend "$RENEW_WINDOW" >/dev/null 2>&1 || {
        echo "[certs] server certificate expires within 30 days — reissuing"
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
    # The enrolment listener's certificate chains to the issuing CA, because the
    # machines that dial it already hold that CA — it arrives with their
    # certificate bundle.
    need_enroll=1
fi

# ── The signer's trust domain ────────────────────────────────────────────────
[ -f "$CERT_DIR/signer-ca.crt" ] && [ -f "$CA_PRIVATE_DIR/signer-ca.key" ] || need_signer_ca=1
if [ "$need_signer_ca" = 0 ]; then
    openssl x509 -in "$CERT_DIR/signer-ca.crt" -noout -checkend 0 >/dev/null 2>&1 || {
        echo "[certs] signer CA has expired — regenerating it and its leaves"
        need_signer_ca=1
    }
fi

[ -f "$CA_PRIVATE_DIR/signer-server.crt" ] && [ -f "$CA_PRIVATE_DIR/signer-server.key" ] || need_signer_server=1
[ -f "$CERT_DIR/signer-client.crt" ] && [ -f "$CERT_DIR/signer-client.key" ] || need_signer_client=1
[ -f "$CERT_DIR/enroll-server.crt" ] && [ -f "$CERT_DIR/enroll-server.key" ] || need_enroll=1

for pair in "$CA_PRIVATE_DIR/signer-server.crt:need_signer_server" \
            "$CERT_DIR/signer-client.crt:need_signer_client" \
            "$CERT_DIR/enroll-server.crt:need_enroll"; do
    f=${pair%%:*}
    [ -f "$f" ] || continue
    openssl x509 -in "$f" -noout -checkend "$RENEW_WINDOW" >/dev/null 2>&1 || {
        echo "[certs] $(basename "$f") expires within 30 days — reissuing"
        case ${pair#*:} in
            need_signer_server) need_signer_server=1 ;;
            need_signer_client) need_signer_client=1 ;;
            need_enroll)        need_enroll=1 ;;
        esac
    }
done

if [ "$need_signer_ca" = 1 ]; then
    need_signer_server=1
    need_signer_client=1
fi

if [ "$need_ca" = 0 ] && [ "$need_server" = 0 ] && [ "$need_console" = 0 ] &&
   [ "$need_signer_ca" = 0 ] && [ "$need_signer_server" = 0 ] &&
   [ "$need_signer_client" = 0 ] && [ "$need_enroll" = 0 ]; then
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
        -days "$SERVER_DAYS" \
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

# ── The signer's certificate authority ────────────────────────────────────────
#
# A SECOND authority, independent of the one above, and that is the whole point
# of it.
#
# The signer issues caller certificates from CA_DIR (the first CA), and every
# leaf it issues is marked for client authentication. If the signer verified
# incoming callers against that same authority, every certificate it had ever
# issued would also be a valid credential FOR it — so caller `backend` could
# dial in with its own legitimate certificate, ask for `payments`, and get it.
# It would then authenticate as `payments`, because atlantis admits any
# CA-signed certificate for a caller whose fingerprint has never been recorded.
#
# So: `ca.*` is what the signer issues FROM, and `signer-ca.*` is what it
# accepts callers BY. Nothing is signed by both. The signer's allowlist of
# common names is the second, independent answer to the same question.
#
# Regenerating this one does not touch the first, and vice versa. They are
# separate trust domains and a caller certificate keeps working across a signer
# CA rotation.
if [ "$need_signer_ca" = 1 ]; then
    echo "[certs] generating the signer's client CA (separate from the issuing CA)..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CA_PRIVATE_DIR/signer-ca.key"
    openssl req -new -x509 \
        -key "$CA_PRIVATE_DIR/signer-ca.key" \
        -out "$CERT_DIR/signer-ca.crt" \
        -days "$DAYS" \
        -subj "/CN=atlantis-signer-clients"
    # The signer verifies clients against this and never mounts CERT_DIR, so it
    # needs its own copy. It has no use for signer-ca.key and does not read it.
    cp "$CERT_DIR/signer-ca.crt" "$CA_PRIVATE_DIR/signer-ca.crt"
fi

# The signer's own server identity, in CA_PRIVATE_DIR because that is the only
# directory the signer mounts.
if [ "$need_signer_server" = 1 ]; then
    echo "[certs] generating the signer's server certificate..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CA_PRIVATE_DIR/signer-server.key"
    openssl req -new \
        -key "$CA_PRIVATE_DIR/signer-server.key" \
        -out /tmp/atl-signer.csr \
        -subj "/CN=atlantis-signer"
    printf 'subjectAltName=DNS:atlantis-signer,DNS:localhost,IP:127.0.0.1\n' > /tmp/atl-signer-san.ext
    openssl x509 -req \
        -in /tmp/atl-signer.csr \
        -CA "$CERT_DIR/signer-ca.crt" \
        -CAkey "$CA_PRIVATE_DIR/signer-ca.key" \
        -CAcreateserial \
        -out "$CA_PRIVATE_DIR/signer-server.crt" \
        -days "$SERVER_DAYS" \
        -extfile /tmp/atl-signer-san.ext
    rm /tmp/atl-signer.csr /tmp/atl-signer-san.ext
fi

# The console's credential TO the signer. CN=atlantis-console, which is what
# SIGNER_ALLOWED_CLIENT_CNS names.
#
# Distinct from console.crt above, which is the console's credential to
# ATLANTIS. Same common name, different authority, different purpose — and they
# must stay distinct: console.crt chains to the issuing CA, so accepting it at
# the signer would reopen exactly the hole this authority exists to close.
if [ "$need_signer_client" = 1 ]; then
    echo "[certs] generating the console's client certificate for the signer..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/signer-client.key"
    openssl req -new \
        -key "$CERT_DIR/signer-client.key" \
        -out /tmp/atl-signer-client.csr \
        -subj "/CN=atlantis-console"
    openssl x509 -req \
        -in /tmp/atl-signer-client.csr \
        -CA "$CERT_DIR/signer-ca.crt" \
        -CAkey "$CA_PRIVATE_DIR/signer-ca.key" \
        -CAcreateserial \
        -out "$CERT_DIR/signer-client.crt" \
        -days "$DAYS"
    rm /tmp/atl-signer-client.csr
fi

# The console's enrolment listener.
#
# Chains to the ISSUING CA, unlike the two above, because the machines that dial
# it are callers — they already hold ca.crt, so they can verify this without
# being given anything extra. First contact is the one moment a machine has no
# certificate of its own; giving it one fewer thing to obtain matters there.
if [ "$need_enroll" = 1 ]; then
    echo "[certs] generating the console's enrolment listener certificate..."
    openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/enroll-server.key"
    openssl req -new \
        -key "$CERT_DIR/enroll-server.key" \
        -out /tmp/atl-enroll.csr \
        -subj "/CN=atlantis-console-enroll"
    ENROLL_SAN="DNS:atlantis-console,DNS:localhost,IP:127.0.0.1"
    if [ -n "$ATLANTIS_DOMAIN" ]; then
        ENROLL_SAN="${ENROLL_SAN},DNS:${ATLANTIS_DOMAIN}"
    fi
    printf 'subjectAltName=%s\n' "$ENROLL_SAN" > /tmp/atl-enroll-san.ext
    openssl x509 -req \
        -in /tmp/atl-enroll.csr \
        -CA "$CERT_DIR/ca.crt" \
        -CAkey "$CA_PRIVATE_DIR/ca.key" \
        -CAcreateserial \
        -out "$CERT_DIR/enroll-server.crt" \
        -days "$SERVER_DAYS" \
        -extfile /tmp/atl-enroll-san.ext
    rm /tmp/atl-enroll.csr /tmp/atl-enroll-san.ext
fi

# ── Permissions ───────────────────────────────────────────────────────────────
# 644 so non-root service containers (atlantis UID 10001, console app user,
# signer's "signer" user) can read keys from the volumes. The security
# boundary here is the *volume mount scope*, not the file mode:
#   - atl-certs is mounted by atlantis (ro) + console (ro) + certs (rw)
#   - atl-ca-private is mounted ONLY by signer (ro) + certs (rw)
# Nothing else in the compose stack ever sees the CA key.
chmod 644 "$CERT_DIR/"*.key "$CERT_DIR/"*.crt
chmod 644 "$CA_PRIVATE_DIR/"*.key "$CA_PRIVATE_DIR/"*.crt
# (Caller-side keys get 600 because they live on a host filesystem, not inside
# a scoped volume.)

echo "[certs] generated:"
ls -la "$CERT_DIR/"
echo "[certs] CA private dir:"
ls -la "$CA_PRIVATE_DIR/"
echo "[certs] done."
