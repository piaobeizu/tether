# gh-13 spike driver

End-to-end driver for the `internal/backend/claude/` package. Runs the
scenarios from spec §8 against a real `claude` binary and emits a
markdown report.

## Quick start

```bash
# Default: run scenarios 1-7 with haiku, no report file
go run ./cmd/spike

# Run a subset
go run ./cmd/spike -scenarios=2,3,4

# Write a markdown report
go run ./cmd/spike -report=cmd/spike/spike-out/$(date -u +%Y-%m-%d)-spike.md

# Use a different model
go run ./cmd/spike -model=sonnet
```

Exit code 0 if all selected scenarios pass (or skip), 1 otherwise.

## Scenarios

| # | scenario | mechanism | typical cost |
|---|---|---|---|
| 1 | full conversation | `claude.New` + `Start` + drain to result | ~$0.005 (haiku) |
| 2 | tool authorization — allow | side-effect prompt + ToolAuthorizer returning Allow | ~$0.01 |
| 3 | tool authorization — deny | same prompt + ToolAuthorizer returning Deny | ~$0.01 |
| 4 | mid-text abort via interrupt | long prompt + `SendInterrupt` mid-stream | ~$0.01 |
| 5 | clean resume | covered by `recover_test.go::TestSession_Recover_RealClaude_Scenario5` | ~$0.05 (in test) |
| 6 | mid-tool-kill + recover | trigger Bash, hit ToolPending, assert Recover refuses (gate works) | ~$0.005 |
| 7 | hook idempotency | deferred — v0.1 ships no tether-owned hook; sub-ticket #3 of plan §9 | n/a |

Scenarios 5 and 7 print SKIP with a pointer; everything else runs against
real claude.

Total cost for a full run is ~$0.05-0.10. The driver caps overall
context at 10 minutes.

## Cost & rate limits

The spike consumes a few cents of API per full pass. If you hit rate
limits or budget concerns, run with `-scenarios=1` (cheapest) to verify
plumbing without incurring tool-flow costs.

## Output

- Standard out: per-scenario PASS/FAIL/SKIP with notes + final summary
- Optional `-report=<path>`: markdown table-of-results, suitable for
  archiving in `cmd/spike/spike-out/` (gitignored except for one
  example)

## Source-of-truth

- spec: [`<ticket-dir>/.specs/2026-04-30-cc-sdk-route.md`](../../../.specs/2026-04-30-cc-sdk-route.md) §8
- plan: [`<ticket-dir>/.plans/2026-04-30-cc-sdk-route.md`](../../../.plans/2026-04-30-cc-sdk-route.md) Step 7
- companion tests: `internal/backend/claude/recover_test.go` (scenarios 5
  and the unit-style refusal cases)

## Model choice notes

Haiku (default) is best for fast iteration — most scenarios complete in
10-20s. Sonnet/Opus are slower (60s+ per scenario) and more expensive
but exercise richer model behavior. The spike runs the same code paths
regardless; the model only affects the LLM responses being decoded.
