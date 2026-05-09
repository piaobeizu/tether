// Package watchdog implements the v0.1 process supervision skeleton from
// spec §4.1 / D-11: the tether CLI's main goroutine plays watchdog over
// two subsystem goroutines (daemon + client). Sub-goroutine panics are
// recovered, deadlocks are detected via heartbeat timeout, and the
// failed subsystem is restarted after exponential backoff.
//
// What this package owns
//
//   - Subsystem — the unit of supervised work. Implementers get an
//     in-loop Heartbeat() func they MUST call periodically; missing the
//     heartbeat for HeartbeatTimeout is treated as a deadlock and the
//     subsystem context is canceled, the goroutine torn down, and a
//     fresh instance restarted.
//
//   - Watchdog.Run — main-goroutine entrypoint. Fires off one
//     superviseLoop per registered Subsystem, blocks until ctx is
//     canceled (typically by the SIGTERM/SIGINT handler the binary
//     installs), then waits for all loops to exit.
//
//   - safeRun / GoSafe — panic-recovering helpers. safeRun wraps a
//     func(ctx) error and turns panics into errors. GoSafe is the
//     "every goroutine in the daemon goes through this" launcher
//     mandated by §4.3 (CI lint rule: no naked `go ...` outside this
//     helper).
//
// What this package does NOT do
//
//   - It does NOT own the cc subprocess (spec §4.2: the PTY master fd
//     belongs to main, not to a subsystem goroutine — sub-goroutine
//     panic must NOT SIGHUP cc). v0.1 daemon-skeleton subsystem
//     manages cc via internal/agent/AgentProvider; a follow-up slice
//     hoists PTY fd ownership to main and threads it down.
//
//   - It does NOT cover the 10% kernel/runtime-fatal scenarios
//     enumerated in §4.1 ("挡不住的"). Those are covered by the daemon
//     resume contract (D-07 / §6) at a different layer.
//
// Spec references: §4.1 (watchdog承诺与限制), §4.2 (resource ownership),
// §4.3 (CI lint), §4.4 (skeleton), §10 PoC-3 (the three scenarios this
// supervisor must survive).
package watchdog
