# web/src/ui

Where tether's differences from vendored CloudCLI UI primitives live.

`web/src/vendor/cloudcli/` holds upstream's files unmodified, and CI enforces
that. This directory is the other half of that arrangement: everything we want
to be *different* from upstream is expressed here, by wrapping, extending or
recomposing what the vendor tree provides.

That split is not tidiness. It is the only reason
`git diff <old-upstream-sha>..<new-upstream-sha>` will still apply to the
vendored files six months from now — which is the whole requirement behind
tether#171.

## What belongs here

- wrappers that re-export a vendored primitive with tether's defaults bound
- components composed out of several vendored primitives
- tether-only primitives with no upstream equivalent
- the tailwind/postcss configs tether actually builds with, extending the
  vendored ones rather than replacing or editing them
- tether's own stylesheet, which `@import`s the vendored `src/index.css` for the
  token definitions and overrides what it needs to — the vendored file is the
  pinned origin of those values, never the place they get adjusted

## What does not

- **edits to a vendored file.** If a vendored primitive is wrong for us, wrap it.
  If wrapping genuinely cannot work, that file has to be detached from the
  absorption chain deliberately — step 6 of `docs/vendoring-cloudcli.md` — not
  edited in place.

## What is here

tether#171 built the vendoring container, its check, and the layer-1 files
upstream's tokens live in. tether#194 is what put the toolchain behind them and
opened the `2-primitives` layer next door.

| file | what it is |
|---|---|
| `tailwind.config.mjs` | the config the build uses: the vendored one, loaded and adjusted |
| `postcss.config.mjs` | the plugin list; `../../postcss.config.mjs` re-exports it |
| `index.css` | tether's stylesheet — `@import`s the vendored one for the tokens |
| `primitives.ts` | the barrel application code imports primitives from |

Every file in that table carries its own reasoning in a header comment, including
the two upstream adoption blockers and the measurements behind how they are
solved. Read the file, not this table — a table is a summary and summaries rot.

## Two things that are easy to get wrong here

**A green `pnpm build` does not mean the CSS pipeline works.** Measured on this
tree: losing the PostCSS config leaves a stylesheet with every design token still
in it and **zero** utility rules, at exit 0; losing the stylesheet's entry point
emits no CSS asset at all, at exit 0. Note what the first one implies — the
tokens survive it, so checking tokens alone would report green. The check that
can tell these apart reads `web/dist`, not the source:
`scripts/check-tailwind-emitted.sh`, wired into CI after the web build and into
`make ci` / `make check-tailwind`.

**jsdom cannot answer the same question.** vitest runs no PostCSS, so
`getComputedStyle` in a test takes the same value whether the toolchain is wired
correctly or absent entirely. Tests here assert primitive *behaviour*; appearance
is the artifact check's business.
