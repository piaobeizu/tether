// Reading a source file as text is how three of this shell's gates work
// (mobileFirst.test.ts and breakpoint.test.ts read `?raw` globs), and every one of
// them has to answer the same question first: is this token a DECLARATION or is it
// a comment talking about one?
//
// 🔴 The answer has to live in exactly one place, because the alternative is two
// strippers that drift — and a stripper is the single most dangerous piece of a
// text-based gate. This module exists only so that the guard test for it
// ("the comment stripper removes comments and nothing else", mobileFirst.test.ts)
// guards every caller rather than one of them.
//
// Nothing in production imports this; it is source-tree machinery for the gates.

/**
 * Strips comments and nothing else.
 *
 * 🔴 Narrow on purpose, because preprocessing is how a self-check comes to hide
 * the defect it is checking for. What is removed is exactly the two comment
 * syntaxes — a `/* … *\/` block and a `//` line comment — and nothing else: no
 * whitespace collapsing, no string removal, no minification. A `100vh` or an
 * `@media (min-width: …)` in a DECLARATION still reaches the caller's assertion,
 * and the mutation proofs for those rules inject one to show that it does.
 *
 * It is needed because the gates' own subject matter is the forbidden tokens.
 * shell.css's header explains why `100dvh` is used "never `100vh`" and why there
 * is no width media query — quoting the very patterns being banned, plus the
 * `grep` commands that check the claims. Without this, every one of those
 * sentences is a false positive, and a rule that cannot be written down beside the
 * code it governs is a rule that stops being written down.
 *
 * ⚠️ The opposite failure is just as real: a comment is not a place to hide a
 * rule, and this function is the reason a real `@media` block wrapped in `/* *\/`
 * would be invisible to the gates. That is correct — a commented-out rule does
 * not ship — but it means "comment it out" is NOT a way to satisfy a gate while
 * keeping the behaviour, because a commented-out rule has no behaviour.
 */
export function stripCssComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^[ \t]*\/\/.*$/gm, '')
}
