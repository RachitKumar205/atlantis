#!/usr/bin/env node
//
// Asserts that package-lock.json still describes the platforms that actually
// install it.
//
// ── Why this exists ─────────────────────────────────────────────────────────
//
// Vite pulls rollup and esbuild, which ship a prebuilt binary per platform as
// optional dependencies. npm 11 on macOS writes a lockfile containing only the
// HOST platform's binaries: one `npm install` under npm 11.17 on darwin-arm64
// took this lockfile from 25 rollup platform entries to 1.
//
// Nothing fails on the machine that did it — that machine has the one binary it
// needs. It fails in CI (linux/x64) and in the Dockerfile spa stage (linux/arm64 musl)
// as `Cannot find module @rollup/rollup-linux-*`, an error whose own text blames
// an unrelated npm bug and tells you to delete node_modules, which does not help.
//
// ── Why a check and not an engines pin ──────────────────────────────────────
//
// `engine-strict` was the first attempt and it is too blunt: it refuses `npm ci`
// as well, and `npm ci` never rewrites the lockfile, so it is safe under any npm.
// Only `npm install` corrupts it. Pinning the tool also polices a proxy — what
// matters is the artifact, and the artifact is checkable directly.
//
// Run it wherever the lockfile changes. It reads one file and needs no packages.

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

// The platforms that install this lockfile, and where each one comes from.
// Add a row when a new consumer appears; do not remove one to make CI pass.
const REQUIRED = [
  { os: 'linux', cpu: 'x64', why: '.github/workflows/ci.yml runs on ubuntu-latest' },
  { os: 'linux', cpu: 'arm64', why: 'the Dockerfile spa stage on an arm64 host' },
]

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const lockPath = join(root, 'package-lock.json')

let lock
try {
  lock = JSON.parse(readFileSync(lockPath, 'utf8'))
} catch (err) {
  console.error(`cannot read ${lockPath}: ${err.message}`)
  process.exit(1)
}

const entries = Object.entries(lock.packages ?? {})
  .filter(([, meta]) => Array.isArray(meta.os) || Array.isArray(meta.cpu))

// A lockfile with no platform-constrained entries at all is not "clean" — it
// means the dependency tree changed shape, and this check would then pass
// vacuously for every platform below. Fail instead, and loudly.
if (entries.length === 0) {
  console.error(
    'package-lock.json contains no platform-specific packages at all.\n' +
      'Either the toolchain no longer ships prebuilt binaries — in which case\n' +
      'delete this check deliberately — or the lockfile is truncated.',
  )
  process.exit(1)
}

const missing = REQUIRED.filter(
  (want) =>
    !entries.some(
      ([, meta]) =>
        (meta.os ?? [want.os]).includes(want.os) &&
        (meta.cpu ?? [want.cpu]).includes(want.cpu),
    ),
)

if (missing.length > 0) {
  console.error('package-lock.json is missing binaries for platforms that install it:\n')
  for (const m of missing) console.error(`  ${m.os}/${m.cpu}  — ${m.why}`)
  console.error(
    '\nThis is what npm 11 does to the lockfile on macOS. Regenerate it with the\n' +
      'npm the consumers use (node 22 ships npm 10.9.x):\n\n' +
      '  rm -rf node_modules package-lock.json\n' +
      '  npx --yes npm@10.9.8 install\n',
  )
  process.exit(1)
}

console.log(
  `package-lock.json covers ${REQUIRED.length} required platforms ` +
    `across ${entries.length} platform-specific packages`,
)
