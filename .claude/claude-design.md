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
    "SkillStage" column is NOT adopted. [CONFIRMED 2026-07-06 by owner xiaokang.w — Shell
    (PTY) is a v2 first-class surface the design never accounted for; a 3-col layout needs
    ≥1200px and a large rebuild. Instead pull the design's SkillStage DAG/skill-progress
    *visuals* into the existing Skills tab — but see backend-gated note in scope_notes.]
  - Settings ships as a right-slide overlay panel, not the design's full-page 240px+1fr nav.
    [CONFIRMED 2026-07-06 — keep the overlay; add an internal mini sub-page nav. The overlay
    + internal tab nav already exists in web/src/Settings.tsx (connection/appearance/about);
    extend it toward the design's account/skills/connection/about set.]
  - "Skills" management (list/install/remove/enable/disable) moves into a Settings "skills"
    sub-page; the right-pane Skills tab keeps only a runtime/read-only glance + a "Manage in
    Settings →" pointer. [CONFIRMED 2026-07-06 — split by purpose: management = config →
    Settings; live progress = work surface → right pane.]
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
  - Skills backend surface (internal/skill/api.go): GET /api/v1/skills (list: id,name,
    description,sourcePath,addedAt) · POST /api/v1/skills (install {name,sourcePath}) ·
    DELETE /api/v1/skills/{id} · POST /api/v1/skills/{id}/{enable|disable} {workspacePath}.
    NO version field, NO update-badge -> the design's skills "version + update badge" are
    cosmetic-only / backend-gated. The old right-pane SkillPane did management via browser
    prompt() and never used enable/disable.
  - BACKEND-GATED (defer, like Pair): a runtime skill "stage" (live DAG/progress stream) has
    no data source distinct from chat fenced blocks (DagBlock ships in §1). So freeze-rule 1's
    "SkillStage visuals in the Skills tab" and freeze-rule 3's "runtime stage in right pane"
    cannot be data-backed yet. Current increment: management -> Settings skills sub-page
    (backend-backed); right-pane Skills tab trimmed to read-only glance + Settings pointer.
  - account sub-page: no user-profile endpoint; only MCP-token API (/api/v1/mcp/tokens
    GET/POST/DELETE) + sign-out (/api/v1/auth/logout) exist. "token (rotate)" maps to MCP
    tokens, not the user's auth token -> keep account minimal/honest, do not fake a rotate.
  - Fenced-block content arrives as an opaque JSON string in FencedBlock.content (wire.gen.ts).
    The UI defines a minimal per-kind schema derived from the design's store shape; the
    daemon-side emission contract (D-19/D-20) is out of scope here.
