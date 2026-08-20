# Examples

## Caller CI is not currently supported

This directory held two GitHub Actions workflows: one running `tide plan` on
every pull request, one running `tide apply` on merge. Both are gone, along with
`guides/set-up-caller-ci.md`, because the mechanism they depended on no longer
exists.

They worked by putting a client certificate and **its private key** into
repository secrets, as `TIDE_TLS_CERT_PEM` / `TIDE_TLS_KEY_PEM` / `TIDE_TLS_CA_PEM`,
and the server address into `ATL_ENDPOINT`. Certificate enrolment replaced that:
a machine generates its own key, sends only a certificate signing request, and
`tide login` writes the result to `~/.atlantis`. Nothing pastes a private key
anywhere, which was the point.

An ephemeral CI runner cannot use that store. It has no state between runs, so
it cannot hold a key, and a certificate it enrolled would be discarded when the
job ended.

**So there is no supported way to run `tide` from CI today.** That is a real
gap, recorded rather than papered over. Designing the replacement — a reusable
enrolment credential, workload identity, something else — is its own piece of
work.

What still holds from the old workflows, and will hold in whatever replaces
them:

| Exit code | Meaning | What CI should do |
|---|---|---|
| 0 | Applied, or nothing to do | Pass |
| 2 | Cross-caller breaking, or waiting for approval | Block the merge; for `apply`, treat as "waiting" rather than "broken" |
| 3 | The schema did not parse | Fail |
| 4 | Destructive | Block; somebody decides whether losing the rows is intended |
