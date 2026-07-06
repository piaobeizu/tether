# Claude Design — target config (tether)

Pins how Claude Design handoffs map into this repo. Consumed by the
`applying-claude-design` skill. Created for tether#3 (2026-07-06).

target_dir: web/src            # React 18 + TS + Vite SPA (v2, web-first pivot)
token_target: web/src/index.css   # design tokens ALREADY match the handoff — no token work

handoff: tether-handoff.zip (2026-07-06) — 6-screen design (desktop main, mobile main,
  pair desktop, pair mobile, settings, error states). NOTE the bundle README's stated
  target ("piaobeizu/tether-app", Tauri, no framework) is STALE — the real target is
  this v2 web SPA. The bundle README's *prose* token values are also stale; the HTML
  `<style id="tokens">` is authoritative and already matches index.css verbatim.

freeze_rules:   # shipped v2 decisions the design must respect unless explicitly revisited
  - Right pane is a 3-tab surface (Chat / Skills / Shell). The design's dedicated middle
    "SkillStage" column is NOT adopted. [UNCONFIRMED — revisit before any desktop-main IA rework]
  - Settings ships as a right-slide overlay panel, not the design's full-page 240px+1fr nav.
    [UNCONFIRMED — revisit before Settings rework]
  - "Skills" currently live in a right-pane SkillPane tab. [UNDECIDED vs a Settings sub-page]
  - Design tokens are final and already in index.css; do NOT re-lift from the bundle
    (identical values; the README prose is wrong). [FINAL]

component_mapping:   # design prototype -> real file
  desktop.jsx Desktop                 -> web/src/App.tsx
  blocks.jsx  Dag(Full|Compact)       -> web/src/fenced-blocks/DagBlock.tsx
  blocks.jsx  Form(Full|Compact)      -> web/src/fenced-blocks/FormBlock.tsx
  blocks.jsx  Candidates(Full|Compact)-> web/src/fenced-blocks/CandidatesBlock.tsx
  blocks.jsx  Media(Full|Compact)     -> web/src/fenced-blocks/MediaBlock.tsx
  mobile.jsx  MobileMain              -> web/src/App.tsx (responsive, partial)
  extras.jsx  Settings                -> web/src/Settings.tsx
  extras.jsx  ErrorStates             -> (net-new: catch-up modal / banner affordances)
  pair.jsx / extras.jsx MobilePairFrame -> (net-new; backend-gated, separate WI)

scope_notes:
  - tether#3 rescoped: tokens DONE; desktop main COVERED. Real work = fenced blocks (first),
    settings sub-pages, error/mobile polish. Pair (desktop+mobile) is backend-gated
    (no internal/pair pkg) -> split into its own WI.
  - Fenced-block content arrives as an opaque JSON string in FencedBlock.content (wire.gen.ts).
    The UI defines a minimal per-kind schema derived from the design's store shape; the
    daemon-side emission contract (D-19/D-20) is out of scope here.
