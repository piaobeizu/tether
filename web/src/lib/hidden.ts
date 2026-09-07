// hidden.ts — which file-tree entries the tree declines to show, and how the
// user changes that (tether#71).
//
// The problem this exists for is not "some directories are ugly". It is that a
// directory's USEFUL entries get pushed below the fold by entries that are
// ephemeral or generated: in one real workspace, 22 of 28 root directories were
// task worktrees, so every file at the root — and the next workspace's row —
// sat under 22 rows of noise.
//
// Three shapes were considered. Honouring .gitignore is the most generic and
// needs no configuration, but ephemeral directories are frequently NOT ignored
// (and the workspace root is often not a git repo at all), so it does not
// actually fix the case that prompted this. Truncating long listings ("first N
// plus a `…K more` row") needs no configuration either, but the server sorts
// directories first and then alphabetically, so a head-truncation keeps the
// noisy middle of the alphabet and hides the tail — the wrong entries. What is
// left is a rule the user can state, which is what this module is.
//
// Two deliberate non-features:
//
//   * No workspace-, tool- or product-specific names in DEFAULT_HIDE_PATTERNS.
//     The defaults are only the directories that are noise in every toolchain.
//     A workspace whose noise has a local shape teaches the tree that shape
//     through the UI; it does not get a special case in here.
//   * Hidden is never gone. The tree renders a "+N hidden" row for any
//     directory with hidden children, so this can only ever cost a click, never
//     a discovery.

/** A file-tree entry, as far as hiding is concerned. */
export interface NamedEntry {
  name: string
}

/**
 * Directory names hidden until the user says otherwise.
 *
 * Deliberately the same set the daemon already refuses to descend into when it
 * builds the @-mention file list (internal/workspace/files.go, skipDirsRecursive
 * — tether#47). Reusing that list rather than inventing a second one keeps the
 * tree and the file picker telling the user the same story about what counts as
 * noise. `.git` is absent because the server never lists it in the first place.
 */
export const DEFAULT_HIDE_PATTERNS: readonly string[] = [
  'node_modules',
  'dist',
  'build',
  'target',
  'vendor',
  '.venv',
  '__pycache__',
  '.next',
  '.cache',
]

/** localStorage key holding the user's pattern list (JSON array of strings). */
export const HIDE_PATTERNS_KEY = 'tether_tree_hidden'

/**
 * Characters that end a "family" prefix, for suggestHidePattern.
 *
 * Names that share a purpose usually share a prefix up to punctuation
 * (`pf.aihub-185`, `build-linux`, `test_utils`), which is what makes a
 * one-click family suggestion possible at all.
 */
const SEPARATORS = '.-_'

/**
 * How many siblings a derived prefix must cover before it is offered as a glob
 * instead of the literal name.
 *
 * Two is a coincidence; three is a pattern. Below this the suggestion is the
 * name itself, so hiding one file never quietly hides its neighbour.
 */
const MIN_FAMILY = 3

const compiled = new Map<string, RegExp>()

/**
 * compile turns a name glob into an anchored RegExp: `*` is any run of
 * characters, `?` is exactly one, and every other character — including regex
 * metacharacters, which appear in real filenames — is literal.
 *
 * Anchored because a substring rule would make `dist` hide `redistribute`, and
 * a rule the user cannot predict is worse than no rule. Results are memoized:
 * this runs once per pattern per visible row on every render.
 */
function compile(pattern: string): RegExp {
  const hit = compiled.get(pattern)
  if (hit) return hit
  let src = '^'
  for (const ch of pattern) {
    if (ch === '*') src += '.*'
    else if (ch === '?') src += '.'
    else src += ch.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  }
  const re = new RegExp(src + '$')
  compiled.set(pattern, re)
  return re
}

/** matches reports whether an entry NAME (never a path) matches one glob. */
export function matches(name: string, pattern: string): boolean {
  if (!pattern) return false
  return compile(pattern).test(name)
}

/** matchesAny reports whether `name` matches any of `patterns`. */
export function matchesAny(name: string, patterns: readonly string[]): boolean {
  return patterns.some(p => matches(name, p))
}

/**
 * partitionEntries splits one directory's entries into what the tree shows and
 * what it folds behind the "+N hidden" row.
 *
 * Order is preserved in both halves — the server already sorted them
 * directories-first-then-alphabetically, and revealing hidden entries should
 * not reshuffle them.
 */
export function partitionEntries<T extends NamedEntry>(
  entries: readonly T[],
  patterns: readonly string[],
): { visible: T[]; hidden: T[] } {
  const visible: T[] = []
  const hidden: T[] = []
  for (const e of entries) {
    if (matchesAny(e.name, patterns)) hidden.push(e)
    else visible.push(e)
  }
  return { visible, hidden }
}

/** hasNonSeparator guards against a prefix made of nothing but punctuation. */
function hasNonSeparator(s: string): boolean {
  for (const ch of s) if (!SEPARATORS.includes(ch)) return true
  return false
}

/**
 * suggestHidePattern picks the pattern the tree's hide button will add when the
 * user clicks it on `name`, given every entry in the same directory (`siblings`
 * INCLUDES `name`).
 *
 * A hide button that could only ever hide one literal name would be useless for
 * the case this feature exists for — twenty-one sibling worktrees means
 * twenty-one clicks — so the button offers a family when it can see one:
 *
 *   * candidates are the prefixes of `name` ending at a separator;
 *   * the candidate covering the MOST siblings wins, ties going to the longest
 *     (most specific) prefix;
 *   * a candidate covering fewer than MIN_FAMILY siblings is not offered at all;
 *   * a candidate that is only punctuation is rejected, so hiding `.claude`
 *     never suggests `.*` and takes every dotfile with it.
 *
 * Fall-through is the literal name, which is always a safe answer. The caller
 * shows the result and the count in the button's tooltip, so the scope of the
 * click is visible before it happens.
 */
export function suggestHidePattern(name: string, siblings: readonly string[]): string {
  let bestPrefix = ''
  let bestCount = 0
  // length - 1: a trailing separator would make the "family" the name itself.
  for (let i = 0; i < name.length - 1; i++) {
    if (!SEPARATORS.includes(name[i]!)) continue
    const prefix = name.slice(0, i + 1)
    if (!hasNonSeparator(prefix)) continue
    const pattern = prefix + '*'
    let count = 0
    for (const s of siblings) if (matches(s, pattern)) count++
    if (count < MIN_FAMILY) continue
    // Iteration is shortest-prefix-first, so `>` alone would lock in the
    // broadest tie. Prefer the more specific prefix when the reach is equal.
    if (count > bestCount || (count === bestCount && prefix.length > bestPrefix.length)) {
      bestPrefix = prefix
      bestCount = count
    }
  }
  return bestPrefix ? bestPrefix + '*' : name
}

/**
 * loadHidePatterns reads the persisted list, falling back to the defaults.
 *
 * An EMPTY stored array is honoured rather than treated as missing: "I want to
 * see everything" is a real preference, and re-seeding the defaults over it
 * would make the last unhide silently undo itself on reload. Anything that is
 * not an array of strings — hand-edited, half-written, from a future version —
 * is discarded in favour of the defaults instead of crashing the tree.
 */
export function loadHidePatterns(): string[] {
  let raw: string | null = null
  try {
    raw = localStorage.getItem(HIDE_PATTERNS_KEY)
  } catch {
    return [...DEFAULT_HIDE_PATTERNS]
  }
  if (raw === null) return [...DEFAULT_HIDE_PATTERNS]
  try {
    const parsed: unknown = JSON.parse(raw)
    if (Array.isArray(parsed) && parsed.every(p => typeof p === 'string')) {
      return parsed as string[]
    }
  } catch {
    /* fall through */
  }
  return [...DEFAULT_HIDE_PATTERNS]
}

/** saveHidePatterns persists the list; a storage failure is not worth a crash. */
export function saveHidePatterns(patterns: readonly string[]): void {
  try {
    localStorage.setItem(HIDE_PATTERNS_KEY, JSON.stringify(patterns))
  } catch {
    /* private mode / quota — the session keeps working, it just won't persist */
  }
}

/**
 * toggleHidePattern adds a pattern or removes it, returning a NEW array.
 *
 * One function for both directions because the tree's control is one button
 * whose meaning flips with the row it sits on, and because "unhide" has to
 * remove the exact pattern that hid the row — not the row's name, which a glob
 * would not match.
 */
export function toggleHidePattern(patterns: readonly string[], pattern: string): string[] {
  if (!pattern) return [...patterns]
  return patterns.includes(pattern)
    ? patterns.filter(p => p !== pattern)
    : [...patterns, pattern]
}

/**
 * hidingPattern returns the pattern currently responsible for hiding `name`, or
 * '' if none is. The unhide button needs this: removing the name would leave a
 * glob like `pf.*` in place and the row would spring straight back.
 */
export function hidingPattern(name: string, patterns: readonly string[]): string {
  return patterns.find(p => matches(name, p)) ?? ''
}
