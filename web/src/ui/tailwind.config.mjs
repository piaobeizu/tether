// The Tailwind config tether actually builds with: the vendored one, loaded and
// then adjusted here. tether#194.
//
// `web/src/vendor/cloudcli/tailwind.config.js` is upstream's file byte-for-byte
// and may not be edited (scripts/check-vendor-provenance.sh hashes its body and
// its header separately, in both directions). Everything this file does is
// therefore either "get that file to load at all" or "change one thing about it",
// and each of those is one block below with the measurement behind it.
//
// ── why .mjs and not .ts, and why postcss loads it rather than vite ─────────
// The load path, which is worth having exactly right because every hop in it is
// load-bearing:
//
//   postcss-load-config searches from vite's `root` (web/) UPWARD and finds
//   web/postcss.config.mjs -> which re-exports web/src/ui/postcss.config.mjs
//   -> which imports THIS file -> which imports the vendored config.
//
// vite.config.ts sets no `css.postcss` key at all; an earlier draft of this
// comment said it pointed at this directory, and it does not. The property that
// matters is the same either way and it is why the config is not loaded from
// vite.config.ts: postcss-load-config `import()`s these files from disk, so
// `import.meta.url` below is really this file's path and the relative specifier
// for the vendored config resolves from web/src/ui/.
//
//   Importing this module from vite.config.ts instead would NOT have that
//   property. Vite bundles vite.config.ts with esbuild into a temp file written
//   *beside* it (web/vite.config.ts.timestamp-*.mjs), inlining relative imports.
//   This file's `import.meta.url` would then be that temp file's URL, one
//   directory up, and '../vendor/cloudcli/...' would resolve outside web/ — a
//   miss, i.e. silently no vendored config.
//
// .mjs and not .ts because nothing transpiles this file: node loads it directly.
// Node 22.18+/24 do strip types from .ts, but relying on that would tie the build
// to a node version for no gain here — this file needs no types.
import { createRequire } from 'node:module'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const WEB_ROOT = fileURLToPath(new URL('../..', import.meta.url))
const VENDOR_CONFIG = new URL('../vendor/cloudcli/tailwind.config.js', import.meta.url)

// ── adoption blocker 1: the vendored config calls require() in an ESM module ─
// `plugins: [require('@tailwindcss/typography')]`. That file is .js inside a
// package with "type": "module", so node evaluates it as ESM, where `require` is
// not bound. Measured on node v24.14.1, from web/:
//
//   await import('./src/vendor/cloudcli/tailwind.config.js')
//     -> ReferenceError: require is not defined in ES module scope
//   require('<abs path to it>')            (from a .cjs file)
//     -> the same ReferenceError, because node >=22.12 executes the ESM body
//        rather than refusing the require outright
//
// An unresolved identifier falls through the scope chain to globalThis, so
// binding `require` there before the module body runs is enough, and is the whole
// fix. Measured: with the binding in place the same import resolves and the
// config arrives with its one plugin present (`__pluginFunction` on it).
//
// Bound around the import and then put back, rather than left in place: this is a
// property of loading one file, not of the process. Nothing is shadowed while it
// is set — CJS modules get `require` as a local binding, so they never see this
// one.
//
// Save-and-restore rather than "inject only if absent". The earlier form skipped
// injection whenever anything had already put a `require` on globalThis, which
// silently handed the vendored config a stranger's resolver with a different base
// — a wrong answer where this wants either the right one or a loud failure.
//
// The base is the VENDORED CONFIG's own URL, not this file's. It only shows up
// today for a relative specifier, and upstream's config has none — but if it ever
// gains `require('./plugins/x')`, resolving that from web/src/ui/ would quietly
// look inside the wrap layer for a file that belongs to the vendor tree. Bare
// specifiers such as '@tailwindcss/typography' resolve identically either way,
// by walking up to web/node_modules.
//
// ⚠️ One-shot: node caches a FAILED module job too. Importing the vendored config
// without the shim throws, and installing the shim afterwards and re-importing in
// the same process throws the identical ReferenceError from the cache. Nothing
// but this file imports that config, so it is unreachable today — but the symptom
// (a correct shim, a permanent error) is confusing enough to be worth the line.
//
// NOT solved by dropping the plugin instead. @tailwindcss/typography is installed
// (web/package.json), because the plugin is load-bearing inside the layer this wi
// starts vendoring: `src/shared/view/ui/Reasoning.tsx:120` at this pin applies
// `not-prose`, a class only that plugin registers. So "the config is the only
// reference to it" — recorded as an inference on tether#194 — is false, and
// dropping the plugin would have changed rendering with nothing going red.
const hadRequire = 'require' in globalThis
const previousRequire = globalThis.require
globalThis.require = createRequire(VENDOR_CONFIG)
let vendorConfig
try {
  vendorConfig = (await import(VENDOR_CONFIG.href)).default
} finally {
  if (hadRequire) {
    globalThis.require = previousRequire
  } else {
    delete globalThis.require
  }
}

if (!vendorConfig || typeof vendorConfig !== 'object') {
  throw new Error(
    `vendored tailwind config at ${VENDOR_CONFIG.href} has no default export; ` +
      'nothing below can be salvaged from that, so fail here rather than three ' +
      'lines down with a TypeError about `content`',
  )
}

// ── the one thing changed about it: content globs are made absolute ──────────
// Upstream ships `content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"]`.
// Tailwind 3's `content.relative` defaults to false, which means a relative glob
// is resolved against the process cwd rather than against the config file that
// declares it. Anchoring the globs to web/ from THIS file's location removes that
// dependency: the set of scanned files is then a property of the repo layout, not
// of where someone launched the build.
//
// ⚠️ Scoped honestly: this is hardening, NOT a bug that was reproduced here. Every
// build path in this repo runs from web/ (`cd web && pnpm build` in ci.yml, the
// same in the Makefile), where the relative globs resolve correctly — and an
// attempt to drive a build from the repo root to demonstrate otherwise never
// reached vite at all (`ERR_PNPM_RECURSIVE_EXEC_NO_PACKAGE`; there is no package
// there). So do not read this as "a build from elsewhere was measured emitting
// nothing". What is measured is the shape of the failure if content ever does
// match nothing: tailwind emits the base layer and no utility rules, and exits 0.
// That is why scripts/check-tailwind-emitted.sh checks utilities and not only
// tokens.
//
// Mapped over upstream's list rather than restated, so a glob upstream adds or
// moves is picked up instead of being silently dropped — derived from the vendored
// value, not a second copy of it.
if (!Array.isArray(vendorConfig.content)) {
  throw new Error(
    'vendored tailwind config no longer declares `content` as an array; ' +
      'rewrite this mapping deliberately rather than letting it resolve to nothing',
  )
}

export default {
  ...vendorConfig,
  content: vendorConfig.content.map((glob) => resolve(WEB_ROOT, glob)),
}
