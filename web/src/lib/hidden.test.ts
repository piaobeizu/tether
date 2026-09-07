// tether#71 — the file tree's hide rules. Pure module, no React: the glob
// semantics and the family suggestion are where the behaviour lives, and both
// are easy to get subtly wrong in ways a rendering test would not name.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  DEFAULT_HIDE_PATTERNS,
  HIDE_PATTERNS_KEY,
  hidingPattern,
  loadHidePatterns,
  matches,
  matchesAny,
  partitionEntries,
  saveHidePatterns,
  suggestHidePattern,
  toggleHidePattern,
} from './hidden'

/**
 * The gmi-ws workspace root that prompted tether#71, in the server's listing
 * order (dirs first, then alphabetical), snapshotted on 2026-08-06: 22 of the
 * 28 directories were ephemeral task worktrees. A snapshot on purpose — the
 * live directory gains and loses worktrees by the hour, which is exactly why
 * the mechanism cannot be a fixed list of names.
 */
const REAL_ROOT = [
  '.claude', '.polyforge', '.repo', '.tmp', 'docs',
  'pf.aihub-185',
  'pf.global-routing-101', 'pf.global-routing-102', 'pf.global-routing-103',
  'pf.global-routing-104', 'pf.global-routing-105', 'pf.global-routing-106',
  'pf.global-routing-107', 'pf.global-routing-108', 'pf.global-routing-112',
  'pf.global-routing-62', 'pf.global-routing-8',
  'pf.ieops-176', 'pf.ieops-237', 'pf.ieops-261', 'pf.ieops-355',
  'pf.ieops-51', 'pf.ieops-57', 'pf.ieops-58', 'pf.ieops-66',
  'pf.silgrid-119', 'pf.tether-71',
  'tether-design-preview',
  '.gitignore', '.polyforge.yaml', 'CLAUDE.md', 'go.work', 'go.work.sum',
]

describe('matches', () => {
  it('is anchored, so a pattern never matches a substring', () => {
    expect(matches('dist', 'dist')).toBe(true)
    expect(matches('redistribute', 'dist')).toBe(false)
    expect(matches('dist-old', 'dist')).toBe(false)
  })

  it('treats * as any run of characters and ? as exactly one', () => {
    expect(matches('pf.ieops-51', 'pf.*')).toBe(true)
    expect(matches('pf.', 'pf.*')).toBe(true) // * may match nothing
    expect(matches('pfx.ieops-51', 'pf.*')).toBe(false)
    expect(matches('a1', 'a?')).toBe(true)
    expect(matches('a', 'a?')).toBe(false)
    expect(matches('a12', 'a?')).toBe(false)
  })

  it('treats regex metacharacters in a pattern as literal filename characters', () => {
    // Without escaping, '.' would match any character and 'a+b' would be a
    // quantifier — both produce hides the user never asked for.
    expect(matches('axb', 'a.b')).toBe(false)
    expect(matches('a.b', 'a.b')).toBe(true)
    expect(matches('aab', 'a+b')).toBe(false)
    expect(matches('a+b', 'a+b')).toBe(true)
    expect(matches('x', '(x)')).toBe(false)
    expect(matches('(x)', '(x)')).toBe(true)
  })

  it('never matches on an empty pattern', () => {
    expect(matches('anything', '')).toBe(false)
    expect(matchesAny('anything', ['', ''])).toBe(false)
  })
})

describe('partitionEntries', () => {
  it('splits on the patterns and preserves the server ordering in both halves', () => {
    const entries = [
      { name: 'src' }, { name: 'node_modules' }, { name: 'lib' },
      { name: 'dist' }, { name: 'README.md' },
    ]
    const { visible, hidden } = partitionEntries(entries, DEFAULT_HIDE_PATTERNS)
    expect(visible.map(e => e.name)).toEqual(['src', 'lib', 'README.md'])
    expect(hidden.map(e => e.name)).toEqual(['node_modules', 'dist'])
  })

  it('hides nothing when the pattern list is empty', () => {
    const entries = [{ name: 'node_modules' }, { name: 'src' }]
    const { visible, hidden } = partitionEntries(entries, [])
    expect(visible).toHaveLength(2)
    expect(hidden).toHaveLength(0)
  })

  it('folds the 22 worktrees out of the real root once pf.* is added', () => {
    const entries = REAL_ROOT.map(name => ({ name }))
    const { visible, hidden } = partitionEntries(entries, ['pf.*'])
    expect(hidden).toHaveLength(22)
    expect(hidden.every(e => e.name.startsWith('pf.'))).toBe(true)
    expect(visible.map(e => e.name)).toEqual([
      '.claude', '.polyforge', '.repo', '.tmp', 'docs', 'tether-design-preview',
      '.gitignore', '.polyforge.yaml', 'CLAUDE.md', 'go.work', 'go.work.sum',
    ])
  })
})

describe('suggestHidePattern', () => {
  it('offers the family glob that covers the most siblings', () => {
    // 'pf.' reaches 22; 'pf.global-' only 11. The broader one wins, which is
    // the whole point — 22 clicks is not a fix.
    expect(suggestHidePattern('pf.global-routing-101', REAL_ROOT)).toBe('pf.*')
    expect(suggestHidePattern('pf.aihub-185', REAL_ROOT)).toBe('pf.*')
  })

  it('never suggests a punctuation-only prefix', () => {
    // '.claude' shares '.' with four other entries. Suggesting '.*' would hide
    // every dotfile from one click on one of them.
    expect(suggestHidePattern('.claude', REAL_ROOT)).toBe('.claude')
    expect(suggestHidePattern('.gitignore', REAL_ROOT)).toBe('.gitignore')
  })

  it('falls back to the literal name when no family reaches the floor', () => {
    // Two is a coincidence: 'README.' covers only README.md itself here.
    expect(suggestHidePattern('README.md', ['README.md', 'LICENSE', 'src'])).toBe('README.md')
    expect(suggestHidePattern('a.txt', ['a.txt', 'b.txt'])).toBe('a.txt')
    expect(suggestHidePattern('docs', REAL_ROOT)).toBe('docs')
  })

  it('prefers the more specific prefix when two reach equally far', () => {
    // Every candidate covers all three, so the narrowest blast radius wins.
    const sibs = ['build-linux-arm', 'build-linux-x86', 'build-linux-ppc']
    expect(suggestHidePattern('build-linux-arm', sibs)).toBe('build-linux-*')
  })

  it('handles names with no separator, and a trailing separator', () => {
    expect(suggestHidePattern('docs', ['docs', 'dist', 'data'])).toBe('docs')
    expect(suggestHidePattern('tmp.', ['tmp.', 'tmp.a', 'tmp.b'])).toBe('tmp.')
  })

  it('supports underscore families too, not just dots and dashes', () => {
    const sibs = ['test_a', 'test_b', 'test_c', 'main.py']
    expect(suggestHidePattern('test_a', sibs)).toBe('test_*')
  })
})

describe('toggleHidePattern', () => {
  it('adds a pattern that is absent and removes one that is present', () => {
    expect(toggleHidePattern(['dist'], 'pf.*')).toEqual(['dist', 'pf.*'])
    expect(toggleHidePattern(['dist', 'pf.*'], 'pf.*')).toEqual(['dist'])
  })

  it('returns a new array rather than mutating the input', () => {
    const before = ['dist']
    const after = toggleHidePattern(before, 'pf.*')
    expect(before).toEqual(['dist'])
    expect(after).not.toBe(before)
  })

  it('ignores an empty pattern', () => {
    expect(toggleHidePattern(['dist'], '')).toEqual(['dist'])
  })
})

describe('hidingPattern', () => {
  it('names the glob responsible, not the entry, so unhiding actually works', () => {
    // Removing 'pf.tether-71' from the list would leave 'pf.*' in place and the
    // row would come straight back.
    expect(hidingPattern('pf.tether-71', ['dist', 'pf.*'])).toBe('pf.*')
    expect(hidingPattern('dist', ['dist', 'pf.*'])).toBe('dist')
    expect(hidingPattern('src', ['dist', 'pf.*'])).toBe('')
  })
})

describe('loadHidePatterns / saveHidePatterns', () => {
  beforeEach(() => localStorage.clear())
  afterEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
  })

  it('seeds the defaults when nothing is stored', () => {
    expect(loadHidePatterns()).toEqual([...DEFAULT_HIDE_PATTERNS])
  })

  it('round-trips a saved list', () => {
    saveHidePatterns(['pf.*', 'dist'])
    expect(localStorage.getItem(HIDE_PATTERNS_KEY)).toBe('["pf.*","dist"]')
    expect(loadHidePatterns()).toEqual(['pf.*', 'dist'])
  })

  it('honours an empty stored list instead of re-seeding the defaults', () => {
    // "show me everything" is a preference; re-seeding would make the last
    // unhide silently undo itself on the next reload.
    saveHidePatterns([])
    expect(loadHidePatterns()).toEqual([])
  })

  it.each([
    ['not json at all', 'not json at all'],
    ['a JSON object', '{"a":1}'],
    ['an array of non-strings', '[1,2,3]'],
    ['a bare string', '"dist"'],
  ])('falls back to the defaults for %s', (_label, raw) => {
    localStorage.setItem(HIDE_PATTERNS_KEY, raw)
    expect(loadHidePatterns()).toEqual([...DEFAULT_HIDE_PATTERNS])
  })

  it('survives a localStorage that throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('denied') })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('denied') })
    expect(loadHidePatterns()).toEqual([...DEFAULT_HIDE_PATTERNS])
    expect(() => saveHidePatterns(['dist'])).not.toThrow()
  })
})

describe('DEFAULT_HIDE_PATTERNS', () => {
  // The one rule the wi is explicit about: no workspace-, tool- or
  // product-shaped names get a back door into the defaults. A user's local
  // noise is taught through the UI, not shipped in the binary.
  it('contains only toolchain-neutral build/dependency directories', () => {
    expect([...DEFAULT_HIDE_PATTERNS]).toEqual([
      'node_modules', 'dist', 'build', 'target', 'vendor',
      '.venv', '__pycache__', '.next', '.cache',
    ])
    for (const p of DEFAULT_HIDE_PATTERNS) {
      expect(p).not.toContain('*') // defaults are exact names, never globs
    }
    // and none of them hides anything in the workspace root that prompted this
    for (const name of REAL_ROOT) {
      expect(matchesAny(name, DEFAULT_HIDE_PATTERNS)).toBe(false)
    }
  })
})
