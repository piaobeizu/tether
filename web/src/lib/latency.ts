// latency.ts — pure EWMA smoothing for /wt/control ping/pong RTT samples.

const ALPHA = 0.3

/**
 * applyLatencySample folds a new RTT sample into the running latency
 * estimate using an exponentially-weighted moving average (alpha=0.3).
 * If prevMs is <= 0 (no estimate yet), the estimate is seeded with the
 * raw sample instead of being smoothed.
 */
export function applyLatencySample(prevMs: number, sampleMs: number): number {
  if (prevMs <= 0) return sampleMs
  return prevMs + ALPHA * (sampleMs - prevMs)
}
