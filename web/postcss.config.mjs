// Loader stub. The config is src/ui/postcss.config.mjs — read that one.
//
// This file exists because of where postcss-load-config looks, and nothing else.
// It searches from vite's `root` (web/) and walks UPWARD to the filesystem root;
// it never descends into src/ui/. So a config placed where tether's wrap layer
// belongs is a config postcss will never find, and the way that failure presents
// itself is a green build whose stylesheet has no utility rules in it at all.
//
// Pointing vite's `css.postcss` at src/ui/ instead would need an absolute path
// computed from node APIs, and web/vite.config.ts deliberately carries no
// @types/node (see its header). One re-export is cheaper than that dependency.
//
// 🔴 Deleting this file does not produce an error. Measured: `pnpm build` then
// exits 0 and emits a 17,044-byte stylesheet that still contains all 42 design
// tokens — they are plain CSS in the vendored file — plus the literal `@tailwind`
// directive, unexpanded, and ZERO utility rules.
//
// That combination is worth reading twice, because it is what makes this the
// nastiest of the failure modes: everything a spot-check would look at is present.
// scripts/check-tailwind-emitted.sh goes red on it, on the utility half rather
// than the token half, and it runs in CI after the web build.
export { default } from './src/ui/postcss.config.mjs'
