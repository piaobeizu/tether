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
// This module is loaded by web/src/ui/postcss.config.mjs, which vite finds via
// `css.postcss` in web/vite.config.ts. That route matters and is not incidental:
//
//   postcss-load-config `import()`s the file from disk, so `import.meta.url`
//   below is really this file's path, and the relative specifier for the vendored
//   config resolves from web/src/ui/.
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
// Bound around the import and then removed, rather than left in place: this is a
// property of loading one file, not of the process. Nothing is shadowed while it
// is set — CJS modules get `require` as a local binding, so they never see this
// one.
//
// NOT solved by dropping the plugin instead. @tailwindcss/typography is installed
// (web/package.json), because the plugin is load-bearing inside the layer this wi
// starts vendoring: `src/shared/view/ui/Reasoning.tsx:120` at this pin applies
// `not-prose`, a class only that plugin registers. So "the config is the only
// reference to it" — recorded as an inference on tether#194 — is false, and
// dropping the plugin would have changed rendering with nothing going red.
const injectedRequire = !('require' in globalThis)
if (injectedRequire) {
  globalThis.require = createRequire(import.meta.url)
}
let vendorConfig
try {
  vendorConfig = (await import(VENDOR_CONFIG.href)).default
} finally {
  if (injectedRequire) {
    delete globalThis.require
  }
}

// ── the one thing changed about it: content globs are made absolute ──────────
// Upstream ships `content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"]`, and
// tailwind resolves relative globs against process.cwd(). That happens to be
// right when the build is started from web/ (`cd web && pnpm build`, which is
// what CI does) and wrong from anywhere else — and wrong here does not mean an
// error. A content glob that matches nothing produces a stylesheet with no
// utility rules at all, exit 0, which is precisely the failure tether#194 exists
// to make visible rather than to inherit.
//
// Anchored to web/ from this file's own location, so the answer no longer depends
// on the caller's cwd. Mapped over upstream's list rather than restated, so a glob
// upstream adds or moves is picked up instead of being silently dropped — the
// expectation is derived from the vendored value, not a second copy of it.
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
