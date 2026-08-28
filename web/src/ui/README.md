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

Empty for now. tether#171 built the vendoring container, its check, and the
layer-1 files upstream's tokens live in (`src/index.css`, `tailwind.config.js`,
`postcss.config.js`); phase 1 of the UI rewrite is what brings in the layer-2
primitives next door and what puts anything in here.
