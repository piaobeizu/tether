/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// This file is the one place in web/ that reads the environment, and the package
// deliberately has no @types/node (tsconfig.node.json has no "types" entry and
// lib is ES2022 only), so `process` is not in scope for tsc. Declaring it here
// keeps that dependency out rather than pulling a whole @types package in for a
// single string compare; being module-scoped, it also does not collide if
// @types/node ever does arrive. vite.config.ts only ever runs under Node.
declare const process: { env: Record<string, string | undefined> }

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  test: {
    environment: 'jsdom',

    // ── the run writes down what failed, whatever the caller did with stdout
    //    (tether#105) ────────────────────────────────────────────────────────
    // This suite has a flake of roughly 5% — 1 red in 18 recorded runs — and it
    // has now been seen twice, by two different people, with NEITHER sighting
    // keeping the name of the test that failed:
    //
    //   8c771c8, 2026-08-18 — the first run of a regression probe went red on
    //     `WorkDetail.test.tsx > falls back to the resume prompt when no
    //     session is bound`. The two reruns after it were green.
    //   aa528e0, 2026-08-18 — the first run in a fresh worktree reported
    //     `Tests 1 failed | 651 passed | 2 skipped`. That run's stdout had been
    //     piped through `grep` for the summary line, so the name was already
    //     gone by the time anyone wanted it. That is why this block exists.
    //
    // Ruled out: 16 consecutive runs in one warm worktree, all green. NOT ruled
    // out: "only the first run of a cold cache". Deleting node_modules/.vite and
    // running cold did make the run measurably colder (7.4s -> 9.04s total,
    // environment 42s -> 52s) and it still passed — n=1, so that hypothesis is
    // neither confirmed nor refuted. Do not repeat it as a finding.
    //
    // A file on disk survives a pipe, a swallowed exit code, a closed terminal
    // and a CI log nobody scrolls back through; the terminal does not. With
    // these two reporters the next sighting names itself even if whoever hits it
    // does nothing but rerun. Two formats on purpose: junit.xml is what CI
    // tooling reads, vitest.json keeps the raw failure text and the per-test
    // durations that XML attributes flatten. Both list every test rather than
    // only the failures, which is what makes diffing a red run against a green
    // one possible at all.
    //
    // Deliberately NOT `retry`. A retried flake reports green, which destroys
    // the one signal this whole block is built to catch. The suite has to stay
    // red on the first red.
    //
    // 'github-actions' is named here even though nothing local uses it. Vitest
    // appends that reporter by itself ONLY while `reporters` is still empty
    // (`if (!resolved.reporters.length)` in its config resolution — vitest
    // 4.1.5, dist/chunks/coverage.DM_a_rWm.js), so writing this array at all
    // would otherwise have silently taken away the inline PR annotations and
    // the job summary CI already had. Measured, not just read: one forced
    // failure under GITHUB_ACTIONS=true emits an `::error` line naming the test
    // with this list, and none at all with the same list minus this entry.
    reporters: [
      'default',
      ...(process.env.GITHUB_ACTIONS === 'true' ? (['github-actions'] as const) : []),
      'junit',
      'json',
    ],
    // Resolved against this file's directory (vitest does
    // `resolve(config.root, outputFile)` and mkdir -p's the parent), so both
    // land in web/test-results/ — gitignored, and held out of the git index by
    // scripts/check-artifacts-uncommitted.sh the same way web/dist is.
    outputFile: {
      junit: 'test-results/junit.xml',
      json: 'test-results/vitest.json',
    },
  },
})
