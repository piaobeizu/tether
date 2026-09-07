#!/usr/bin/env bash
# check-tailwind-emitted.sh — assert the CSS pipeline actually ran, on the artifact.
#
# Why this exists (tether#194). `pnpm build` going green proves nothing about
# Tailwind. Every way of getting the wiring wrong is silent:
#
#   - no postcss config found        -> tailwind never runs
#   - tailwind runs with NO config   -> its built-in defaults; no vendored tokens
#   - content globs match nothing    -> base layer emitted, zero utility rules
#   - the vendored index.css has no  -> no @tailwind directives reached postcss,
#     entry point reaching it           so no CSS asset at all
#
# Not one of those is an error. PostCSS with a misconfigured Tailwind emits a
# stylesheet, exits 0, and the build succeeds — so the only place the difference
# is visible is web/dist. Hence a check that reads the artifact.
#
# ── how the expectations are derived, and why that matters ───────────────────
# The tempting shape is a list of class names and token names written down here.
# That is the WIRE-8 defect this repo already paid for once (see the header of
# check-vendor-provenance.sh): a gate whose expectation is a hand-maintained copy
# of the thing it checks passes forever while blind, because the copy is what
# rots.
#
# So nothing here is a copy:
#
#   tokens     the custom-property NAMES come out of the vendored src/index.css.
#   values     the expected CSS values come out of the vendored tailwind config,
#              loaded through web/src/ui/ — the same loader the build uses, so a
#              wrap that cannot load makes this red rather than vacuous.
#   usage      each probe class is confirmed present in vendored source first. A
#              class nothing uses is legitimately absent from the artifact
#              (tailwind emits on demand), so without this the gate could pass by
#              checking nothing.
#
# ── the assertion that carries the most ──────────────────────────────────────
# `.rounded-md` must emit `calc(var(--radius) - 2px)`. Default Tailwind emits
# `border-radius:0.375rem` for that class. So this one comparison separates three
# states a build cannot otherwise tell apart: tailwind did not run (no rule),
# tailwind ran on defaults (a rule with the wrong value), tailwind ran on the
# vendored config (the value below). "Tailwind ran" is not the property worth
# checking; "tailwind ran on OUR config" is.
set -euo pipefail
cd "$(dirname "$0")/.."

WEB=web
VENDOR="$WEB/src/vendor/cloudcli"
TOKENS_CSS="$VENDOR/src/index.css"
PRIMITIVES="$VENDOR/src/shared/view/ui"

fail=0
red() { echo "FAIL: $*" >&2; fail=1; }

command -v node >/dev/null 2>&1 || { echo "missing required tool: node" >&2; exit 2; }

# ── the artifact ─────────────────────────────────────────────────────────────
# web/dist is not in git (tether#81), so this script is meaningless before
# `pnpm build`. Say which of the two it is rather than reporting a content
# failure for a missing directory.
[[ -d "$WEB/dist" ]] || { echo "$WEB/dist does not exist — run 'cd web && pnpm build' first" >&2; exit 2; }

mapfile -t css_files < <(find "$WEB/dist" -type f -name '*.css' | sort)
if (( ${#css_files[@]} == 0 )); then
	red "no .css file under $WEB/dist."
	echo "      vite emits one only if something reachable from an entry point" >&2
	echo "      imports a stylesheet. web/index.html links /src/ui/index.css; if" >&2
	echo "      that link is gone, the vendored design tokens ship in nothing." >&2
	echo >&2
	echo "The CSS pipeline is not wired. See web/src/ui/tailwind.config.mjs." >&2
	exit 1
fi
css=$(cat "${css_files[@]}")
echo "reading ${#css_files[@]} css file(s) under $WEB/dist ($(printf '%s' "$css" | wc -c) bytes)"

# ── 1. design tokens: every custom property the vendored stylesheet declares ──
# These live in `@layer base { :root { ... } }`, which tailwind emits
# unconditionally — unlike utilities, they are not usage-gated. So "all of them"
# is the right strength here, and a subset arriving means the vendored stylesheet
# was partially processed rather than imported.
mapfile -t want_tokens < <(
	grep -oE '^[[:space:]]*--[a-z0-9-]+:' "$TOKENS_CSS" | tr -d ' :' | sort -u
)
if (( ${#want_tokens[@]} == 0 )); then
	red "$TOKENS_CSS declares no custom properties at all."
	echo "      The token assertion below would pass against an empty set, which" >&2
	echo "      is the blind-gate shape this script exists to avoid. If upstream" >&2
	echo "      really moved its tokens, point this at wherever they went." >&2
else
	missing=()
	for t in "${want_tokens[@]}"; do
		[[ "$css" == *"$t:"* ]] || missing+=("$t")
	done
	if (( ${#missing[@]} )); then
		red "${#missing[@]} of ${#want_tokens[@]} design token(s) declared in $TOKENS_CSS"
		echo "      are not defined anywhere in the built CSS:" >&2
		printf '        %s\n' "${missing[@]}" >&2
		echo "      The vendored stylesheet is the pinned origin of these values." >&2
		echo "      Check that web/src/ui/index.css still imports it and that" >&2
		echo "      something reachable from an entry point still pulls that in." >&2
	else
		echo "OK: all ${#want_tokens[@]} design token(s) from $TOKENS_CSS are defined in the built CSS"
	fi
fi

# ── 2. utilities, with the values the VENDORED config asks for ────────────────
# Pairs of (class, expected substring), both read out of the loaded config rather
# than written here. Loading it through the wrap is deliberate: if
# web/src/ui/tailwind.config.mjs stops loading — the require() shim breaking is
# the obvious way — this exits non-zero instead of skipping the check.
probes=$(
	cd "$WEB" && node --input-type=module -e '
		const cfg = (await import("./src/ui/tailwind.config.mjs")).default
		const ex = cfg.theme?.extend ?? {}
		const out = []
		const radius = ex.borderRadius ?? {}
		for (const [k, v] of Object.entries(radius)) out.push(`rounded-${k}\t${v}`)
		const colors = ex.colors ?? {}
		for (const [k, v] of Object.entries(colors)) {
			const val = typeof v === "string" ? v : v?.DEFAULT
			if (typeof val === "string") out.push(`bg-${k}\t${val}`)
		}
		process.stdout.write(out.join("\n"))
	'
)

# The class tokens vendored source actually uses, as whole words.
#
# Substring matching is wrong here and the first version of this script had it:
# `bg-accent` appears in Button.tsx only inside `hover:bg-accent` and
# `active:bg-accent/80`. Those are variants, so tailwind emits
# `.hover\:bg-accent:hover{...}` and no bare `.bg-accent{}` rule at all — and the
# gate then reported a broken pipeline on a working one. Splitting into tokens on
# quotes and commas only (NOT on ':' or '/') keeps `hover:bg-accent` and
# `bg-accent/80` as single tokens, so neither can pass for a bare use of
# `bg-accent`.
mapfile -t used_classes < <(
	cat "$PRIMITIVES"/* | tr '"'"'"'`,' '    ' | tr -s ' \t' '\n\n' | sort -u
)
if (( ${#used_classes[@]} == 0 )); then
	red "no class tokens could be read out of $PRIMITIVES."
	echo "      Every utility probe below would then be skipped as unused, and the" >&2
	echo "      gate would report a green it did not earn." >&2
fi

n_checked=0
while IFS=$'\t' read -r cls want; do
	[[ -n "$cls" && -n "$want" ]] || continue

	# Tailwind emits on demand, so a class no source uses is legitimately absent.
	# Confirm bare usage first; a probe with no bare use is skipped, not passed.
	printf '%s\n' "${used_classes[@]}" | grep -qxF -- "$cls" || continue

	n_checked=$(( n_checked + 1 ))
	rule=$(printf '%s' "$css" | grep -oE "\.${cls}\{[^}]*\}" | head -1 || true)
	if [[ -z "$rule" ]]; then
		red ".$cls is used in $PRIMITIVES but no rule for it is in the built CSS."
		echo "      Tailwind emits utilities on demand by scanning the content globs," >&2
		echo "      so an absent rule means the globs did not reach that source. They" >&2
		echo "      are made absolute in web/src/ui/tailwind.config.mjs precisely" >&2
		echo "      because upstream's relative ones silently match nothing when the" >&2
		echo "      build runs from anywhere but web/." >&2
	elif [[ "$rule" != *"$want"* ]]; then
		red ".$cls emits a value the vendored config did not ask for."
		echo "        built:    $rule" >&2
		echo "        expected to contain: $want" >&2
		echo "      This is what tailwind running on its own DEFAULTS looks like: a" >&2
		echo "      rule is present, so nothing is missing and nothing is red, but the" >&2
		echo "      vendored theme is not in effect. web/src/ui/postcss.config.mjs has" >&2
		echo "      the reason the vendored postcss.config.js cannot be used directly." >&2
	fi
done <<<"$probes"

if (( n_checked == 0 )); then
	red "no utility probe could be checked at all."
	echo "      Either the vendored tailwind config declares no theme.extend" >&2
	echo "      borderRadius/colors entries, or no vendored primitive uses any of" >&2
	echo "      them. Both leave this half of the gate asserting nothing." >&2
else
	echo "OK: $n_checked utility class(es) carry the value the vendored config declares"
fi

if (( fail )); then
	echo >&2
	echo "The CSS pipeline is not doing what it claims. web/src/ui/tailwind.config.mjs" >&2
	echo "and web/src/ui/postcss.config.mjs carry the wiring and the reasons." >&2
	exit 1
fi
