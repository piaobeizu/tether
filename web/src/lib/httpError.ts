// httpError.ts — turning a non-2xx response into the sentence a user reads.
//
// Every fetch in this SPA used to answer a refusal with `HTTP ${res.status}` and
// drop the body on the floor. That is the last hop of tether#147 and tether#159,
// whose whole output is careful refusal wording the daemon derives from the
// sentinel it actually hit — "workspace: that path is not a directory",
// "skill: a skill source must be absolute" — and none of it reached a screen. The
// backend was right, the tests were green, and for the user it was a no-op.
//
// One module rather than a line at each call site: seven copies of "read the
// body, decide what it means, cap it" is seven chances to differ, and the two
// that matter here (a body that is JSON, a body that is enormous) are exactly
// the ones a hurried copy leaves out. That is also tether#173's AC-R9 clause 1 —
// one response-handling path, one shared wording extractor, no per-call-site
// reimplementation.
//
// Ported to the phase-1 tree by tether#188. It arrived with ZERO call sites (the
// old SPA's ten went with tether#174) and now has one: lib/fileTreeCache.ts.

/**
 * The longest daemon message that reaches the screen intact.
 *
 * The daemon's own refusals are one short sentence each, so this is not sized for
 * them — it is sized against the assumption that they are all it can ever send.
 * A proxy's HTML 502 page, a stack trace, a mis-routed file: those arrive on the
 * same channel, and this error text lands in a one-line `<div>` next to a form.
 */
export const HTTP_ERROR_MAX_CHARS = 300

/**
 * Appended when a body is cut, so a truncated message SAYS it was truncated
 * rather than merely stopping mid-word — which reads as the daemon having said
 * something incoherent.
 */
export const HTTP_ERROR_TRUNCATED = '… (truncated)'

/**
 * JSON fields consulted, in order, for the human-readable message.
 *
 * Order is precedence and not preference: a body carrying both `error` and
 * `message` is answering two different questions, and the daemon's HTTP handlers
 * write the sentence under `error` where they write JSON at all.
 */
const MESSAGE_FIELDS = ['error', 'message', 'detail', 'reason'] as const

/**
 * The status-only sentence — what all ten call sites in the pre-tether#174 SPA
 * used to throw, and what is still correct when there is genuinely nothing else
 * to say.
 *
 * Exported so a test can name the value the defect produced rather than spelling
 * it again: a test that only asserts the new wording is present passes on the old
 * build the moment the wording appears for some other reason.
 *
 * Only ONE file actually takes it up. web/test/workspace-tree.spec.ts derives its
 * expected fallback from this function, so its tether#161 arms move with the
 * wording. httpError.test.ts does NOT: its fallback cases spell the expected
 * string out as a `'HTTP <n>'` literal, and its single reference to
 * httpStatusFallback is a `.not.toBe`, i.e. naming the value that must NOT
 * appear. So changing the wording here reddens httpError.test.ts rather than
 * being absorbed by it.
 *
 *   Check: change this body to `Status ${status}` and run, from web/,
 *   `pnpm vitest run src/lib/httpError.test.ts test/workspace-tree.spec.ts` —
 *   workspace-tree.spec.ts stays fully green, httpError.test.ts goes red.
 *   `grep -n "toBe('HTTP\|httpStatusFallback" src/lib/httpError.test.ts` shows
 *   the same split without running anything.
 *
 * ⇒ Deriving those literals from httpStatusFallback is an open cleanup, left
 *   undone by tether#190 because httpError.test.ts is outside its declared
 *   resources. Until then the instruction is descriptive, not established
 *   practice: a NEW assertion should derive, and if you are already editing an
 *   old one, convert it.
 */
export function httpStatusFallback(status: number): string {
  return `HTTP ${status}`
}

/** Collapse a body's whitespace so it renders as one line and compares exactly. */
function flatten(text: string): string {
  return text.replace(/\s+/g, ' ').trim()
}

/**
 * The message a JSON body carries: '' when it parses but says nothing a human can
 * read, and null when it is not JSON at all.
 *
 * Three outcomes and not two, because '' and null need opposite handling. A body
 * that is not JSON is a plain-text refusal and must be shown verbatim; a body that
 * IS JSON but carries no message field must fall back to the status rather than
 * put `{"code":17,"retryable":false}` in front of a user.
 */
function messageFromJSON(body: string): string | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(body)
  } catch {
    return null
  }
  // A bare JSON string is a message — `http.Error` never writes one, but
  // `json.NewEncoder(w).Encode("...")` does.
  if (typeof parsed === 'string') return flatten(parsed)
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return ''
  const obj = parsed as Record<string, unknown>
  for (const key of MESSAGE_FIELDS) {
    const value = obj[key]
    if (typeof value !== 'string') continue
    const flat = flatten(value)
    if (flat !== '') return flat
  }
  return ''
}

/**
 * What to show for `status`, given the raw body bytes. The pure half, so the
 * decisions (JSON vs text, empty, over-long) are testable without a Response.
 */
export function httpErrorText(status: number, body: string): string {
  const fromJSON = messageFromJSON(body)
  const text = fromJSON === null ? flatten(body) : fromJSON
  if (text === '') return httpStatusFallback(status)
  if (text.length <= HTTP_ERROR_MAX_CHARS) return text
  return text.slice(0, HTTP_ERROR_MAX_CHARS) + HTTP_ERROR_TRUNCATED
}

/**
 * Minimum a caller has to hand over. `Response` satisfies it; so does the plain
 * object a fetch stub returns, including stubs with no `text` at all — those fall
 * back to the status, which is the behaviour they were written against.
 *
 * `text` being optional is load-bearing, not lenience: web/test/workspace-tree.spec.ts
 * asserts both arms against each other (a source WITH `text` must yield the
 * daemon's sentence, one WITHOUT must yield httpStatusFallback). Making it
 * required would turn every pre-tether#161 stub from "degrades" into "rejects".
 */
export interface HttpErrorSource {
  status: number
  text?: () => Promise<string>
}

/**
 * The message to put in the Error for a response the caller has already decided
 * is a failure. Reads the body — so only call it on the branch that is NOT going
 * to read it as JSON, which is every `!res.ok` branch in this SPA.
 *
 * Never rejects. An error path that can itself fail is an error path that hides
 * the error it was reporting, so a body that cannot be read at all degrades to
 * the status rather than replacing the daemon's refusal with a TypeError.
 */
export async function httpErrorMessage(res: HttpErrorSource): Promise<string> {
  let body: unknown
  try {
    const read = res.text
    body = typeof read === 'function' ? await read.call(res) : ''
  } catch {
    return httpStatusFallback(res.status)
  }
  if (typeof body !== 'string') return httpStatusFallback(res.status)
  return httpErrorText(res.status, body)
}
