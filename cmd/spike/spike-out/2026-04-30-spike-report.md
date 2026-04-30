# spike report — gh-13 cc-sdk-route

- generated: 2026-04-30T10:10:52Z
- model: `haiku`
- spec: `.specs/2026-04-30-cc-sdk-route.md` §8

| # | scenario | result | duration | notes |
|---|---|---|---|---|
| 1 | full conversation | PASS | 10.113s | stop_reason=end_turn sid=bce86792-b1bf-4ccd-83bf-f03c0259bf18 |
| 2 | tool authorization — allow | PASS | 17.02s | scenario 2 (allow) — auth invoked, stop=end_turn, is_error=false |
| 3 | tool authorization — deny | PASS | 19.563s | scenario 3 (deny) — auth invoked, stop=end_turn, is_error=false |
| 4 | mid-text abort via interrupt | PASS | 7.185s | stop_reason= terminal=aborted_streaming is_error=true |
| 5 | clean resume — see recover_test.go | SKIP | 0s | verified in: recover_test.go::TestSession_Recover_RealClaude_Scenario5 |
| 6 | mid-tool-kill + recover | PASS | 13.979s | Recover correctly refused with ErrUnsafeToReload while ToolPending |
| 7 | hook idempotency | SKIP | 0s | deferred: hook idempotency requires a tether-owned hook; v0.1 ships none — deferred to sub-ticket #3 in plan §9 |
