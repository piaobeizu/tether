.PHONY: all build codegen test go-test web-test check-artifacts check-vendor \
        check-vendor-diff verify-vendor-upstream ci release clean

BINARY  := bin/tether
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Make the make targets hermetic against an enclosing Go workspace (tether#177).
#
# This only bites when the checkout sits *inside* someone else's workspace, and
# in the polyforge workspace both checkouts do: a task worktree and the canonical
# .repo/<repo> clone each resolve GOWORK to the workspace-root go.work, which
# `use`s other repos there and not this module. It is not only the pf.* trees.
# `go` then resolves in workspace mode against a workspace this module is not
# part of, and every target that shells out to `go` fails:
#
#   make codegen  ->  go: no such tool "tygo"
#   make go-test  ->  pattern ./...: directory prefix . does not contain modules
#                     listed in go.work or their selected dependencies
#
# The codegen one is the trap: it names a *tool*, so it reads as a missing
# dependency and invites `go install .../tygo`. The tool is not missing — go.mod
# declares it and `GOWORK=off go tool` lists it. Single-module resolution is what
# is missing. This used to be a hand-maintained rule in two step templates, i.e.
# maintained by whoever remembered it.
#
# CI never invokes make (it runs scripts/*.sh and `go` directly), so this reaches
# local make targets only. In an *unmodified* clone `off` merely restates what
# `go` already concludes. Once you write your own go.work it no longer restates —
# it overrides — and the two shapes of workspace differ sharply:
#
#   use-only:     a benefit. The package set of `make go-test` stays exactly this
#                 module's, identical to CI's, instead of drifting with local
#                 workspace state. (You cannot `use` both this module and v0/
#                 regardless: v0/go.mod declares the same module path, so go
#                 rejects the workspace outright — "module
#                 github.com/piaobeizu/tether appears multiple times in
#                 workspace".)
#   with replace: SILENTLY IGNORED, and this is the case to know about. Pointing
#                 a dependency at a local fork is the usual reason to write a
#                 go.work, and every make target drops it: `make build` exits 0
#                 and links the upstream module, with no warning and no error.
#                 Measured by pointing a direct dependency at a local fork — the
#                 recipe resolves the version go.mod pins, a bare `go` in the
#                 same directory resolves the fork.
#
# `?=` yields to any outer value, so the way back in is to pass one explicitly:
#
#   GOWORK="$PWD/go.work" make build    # ABSOLUTE path required; a relative one
#                                       # dies with `invalid GOWORK: not an
#                                       # absolute path`
#
# `export` is load-bearing — a plain make variable is not in the recipe's
# environment, which is where `go` reads it. `?=` does not assign to a
# set-but-empty variable, so `GOWORK= make build` hands the recipe an empty
# GOWORK, `go` falls back to auto-discovery, and the original failure comes back.
# That is an unhandled edge, not a designed control: nothing exercises it.
#
# Scope: make targets only. A bare `go test ./...` in the shell is still on you.
export GOWORK ?= off

# `make build` is the only supported build. web/dist (the embedded SPA) is not in
# git, so until it has been built every go command in this module — including
# `go list ./...`, hence gopls — fails with "pattern all:dist: no matching files
# found". Run `make build` once after cloning. See web/embed.go for the why.

# Must stay in sync with .github/workflows/ci.yml. See the comment there for why
# -race and -count=1 are the baseline rather than an opt-in.
#
# `make test` and `make go-test` are what read this. `make ci` no longer does —
# it runs the workflow's own "Run Go tests" line, so its agreement with CI is
# checked rather than asserted. This one is still asserted: nothing compares the
# two, and the flags could drift apart without anything reddening.
GOTEST  := go test -count=1 -race

all: build

build:
	VERSION="$(VERSION)" bash scripts/build.sh

codegen:
	bash scripts/codegen.sh

# check-vendor is in here and not only in `ci` because `make test` is what anyone
# actually runs before pushing, and a gate that only exists in CI is a gate you
# meet after the fact. It needs neither Go nor node, so it costs nothing here.
test: check-vendor go-test web-test

go-test:
	$(GOTEST) ./...

# Leaves web/test-results/{junit.xml,vitest.json} behind — every run, pass or
# fail. Read those instead of the terminal when a run goes red: this suite has a
# ~5% flake (tether#105) whose two sightings so far both lost the name of the
# failing test, once because the output had been piped into `grep`. The files
# survive that; they are gitignored and rewritten by the next run, so copy one
# aside before rerunning. web/vite.config.ts has the full record.
web-test:
	cd web && pnpm test

# Asserts web/dist and the tsc caches are absent from the git index and still
# ignored. Also run by CI — see the comment in scripts/check-artifacts-uncommitted.sh.
check-artifacts:
	bash scripts/check-artifacts-uncommitted.sh

# Asserts every file under web/src/vendor/cloudcli is still byte-identical to the
# upstream tag its provenance header names (tether#171). Offline. See
# docs/vendoring-cloudcli.md.
check-vendor:
	bash scripts/check-vendor-provenance.sh check

# The other half of the offline gate, and the half that reads the *change* rather
# than the state: a content hash that moved while its tag/sha/status did not is an
# in-place edit that was rehashed, which `check` is structurally unable to see
# (after the rehash the state is consistent). CI runs this per PR against the PR
# base; locally, BASE is whatever you are stacked on.
#
#   make check-vendor-diff BASE=@origin/main
#
# BASE and HEAD take '@<git-ref>' or a path to a manifest file.
BASE ?= @origin/main
check-vendor-diff:
	bash scripts/check-vendor-provenance.sh check-diff "$(BASE)" $(HEAD)

# The network half — "are the recorded hashes what upstream actually published at
# the recorded sha". Step 5 of an absorption; not a per-PR gate, because a gate
# that reddens when github.com has a bad afternoon is one people rerun past.
#
#   make verify-vendor-upstream CLONE=/tmp/ccui
verify-vendor-upstream:
	@test -n "$(CLONE)" || { echo "usage: make verify-vendor-upstream CLONE=<upstream-clone>"; exit 2; }
	bash scripts/check-vendor-provenance.sh verify-upstream "$(CLONE)"

# ── `make ci` reads ci.yml instead of restating it ───────────────────────────
#
# This target used to be a hand-written list of what CI does, kept beside the
# workflow and compared against it by nobody. It had drifted, and not harmlessly:
# the cross-compile gate and the opt-in-test type-check were both in ci.yml and in
# no make target at all, so a green `make ci` asserted a coverage it did not have.
# ci.yml's comment on the cross-compile step blames that gate's earlier absence
# for v0.4.1 and v0.5.0 being tagged with no GitHub Release (tether#85). Read that
# as the workflow's own account rather than a settled cause: tether#87 records the
# release path failing across a longer run of tags than those two, so the missing
# gate cannot be the whole of it.
#
# Restating the list correctly would only have reset the clock. A copy that agrees
# today is still a copy: the next step added to ci.yml reopens the same hole in the
# same silence, and the reason there was nothing to notice the first time is that
# nothing was reading both sides. So `make ci` no longer stores commands. It reads
# ci.yml, and for every step of every job there it either runs that step's own
# `run:` body verbatim or finds the step named in CI_STEP_PLAN with a reason not
# to. Both directions are checked before anything executes: a step ci.yml has and
# CI_STEP_PLAN does not name is an error, and a CI_STEP_PLAN entry naming a step
# ci.yml no longer has is also an error. Adding a step to CI and not here reddens
# `make ci` — which is the property the old arrangement never had.
#
# What that does and does not buy, stated exactly, because the difference is the
# whole value of the thing:
#
#   executed, so it cannot drift  the `run:` body and `env:` of every step this
#                                 file marks `run`. They are read out of ci.yml
#                                 and handed to a shell; there is no second copy
#                                 to disagree with.
#   checked, so drift reddens     which steps and jobs exist, and that each has a
#                                 disposition. Job and step names are copied here
#                                 and are the join key, so a rename reddens too —
#                                 loudly, and on purpose.
#   neither                       the skip reasons. They are prose, nothing reads
#                                 them, and a step can keep its name while the
#                                 sentence beside it quietly stops being true.
#                                 Also the schema itself: the driver whitelists
#                                 the workflow keys it understands and reddens on
#                                 the rest, which converts "silently ignored" into
#                                 "teach me first", but it is not the same as
#                                 reproducing them.
#
# The cost, stated rather than hidden: this needs a real YAML parser. Reading a
# workflow with grep or sed is the very failure this target exists to end — a
# pattern that stops matching a reworded step goes quiet, and quiet reads as green.
# So PyYAML is required, and its absence fails `make ci` with an install line
# rather than skipping the reconciliation.
#
# `make ci` now costs what CI costs, cross-compile and all. `make test` is the
# fast pre-push loop and does not pretend otherwise.
#
# One consequence of running CI's steps rather than `make build`'s: the bin/tether
# left behind is built by ci.yml's own `go build`, which passes no -ldflags, so it
# carries no VERSION stamp. That is what CI produces and therefore what belongs
# here, but it does overwrite a stamped binary from `make build`. Rebuild with
# `make build` if you wanted the stamped one.
#
# Scope is ci.yml. .github/workflows/release.yml is a different gate on a
# different trigger and is not reproduced here.
CI_WORKFLOW := .github/workflows/ci.yml
export CI_WORKFLOW

# One line per step in CI_WORKFLOW: <run|skip>|<job>|<step key>|<reason>
#
# The step key is the step's `name:`, or `uses:<action>` for a step that has none.
# `run` entries take no reason; `skip` entries must give one, because "we do not
# run this locally" is a claim that ages and the reason is what lets the next
# reader tell a deliberate omission from a forgotten one.
#
# ⚠ A skip reason is prose, and nothing checks it. The reconciliation proves the
# step still exists and is still dispositioned; it cannot prove the sentence next
# to it is still true. Keep them free of anything with a version or a flag in it,
# so that what rots is at worst the wording and never a fact.
define CI_STEP_PLAN_BODY
skip|build|uses:actions/checkout@v4|your working tree is the checkout
skip|build|Set up Go|the Go toolchain you already have stands in for the action
skip|build|Set up Node + pnpm|the Node you already have stands in for the action
skip|build|Install pnpm|installing pnpm globally would replace the one on your PATH
skip|build|Install tygo|scripts/codegen.sh reaches tygo as a go tool from go.mod
run|build|Check no generated artifacts committed|
run|build|Check vendored upstream provenance|
skip|build|Check vendored provenance diff against PR base|it reads the PR base commit, which no local run has; make check-vendor-diff is the hand-run half
run|build|Run codegen|
run|build|Check codegen drift|
run|build|Install web dependencies|
run|build|Build web|
run|build|Stamp SPA bundle|
run|build|Build Go binary|
run|build|Check binary embeds the SPA just built|
run|build|Cross-compile every released platform|
run|build|Run wire-contract tests|
skip|build|Upload web test report|uploads into the workflow run; there is no local run to upload to
run|build|Run Go tests|
run|build|Type-check opt-in real-binary tests|
endef

# Reconcile CI_STEP_PLAN against CI_WORKFLOW, then run what CI runs, in CI order.
#
# Every '$' below is written '$$' so make hands the literal through to python.
# There is exactly one, in the GitHub-expression guard, and the probe that
# mutates a run: body to contain an expression is what keeps that true.
define CI_DERIVE_BODY
import os, subprocess, sys

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "make ci: needs PyYAML to read the workflow it derives its work from.\n"
        "  try:  python3 -m pip install pyyaml\n"
        "  a distro that manages its own python packages (PEP 668) refuses that;\n"
        "  there, install the distro package (python3-yaml) or use a virtualenv.\n"
        "  not grep: a pattern that stops matching a reworded step goes quiet,\n"
        "  and this target exists because a quiet gate reads as a passing one.\n")
    sys.exit(2)

workflow = os.environ["CI_WORKFLOW"]
if not os.path.exists(workflow):
    sys.stderr.write(
        "make ci: no " + workflow + " under " + os.getcwd() + ".\n"
        "  This target reads the workflow by a repo-relative path, so run make\n"
        "  from the repository root. There is nothing to reconcile against here.\n")
    sys.exit(2)
with open(workflow) as fh:
    parsed = yaml.safe_load(fh) or {}


def spelling(key):
    """YAML 1.1 resolves the bare words on/off/yes/no to booleans, so the
    workflow's `on:` key arrives as True. Put the source spelling back before
    comparing against anything, or the top-level check rejects every workflow
    ever written."""
    if key is True:
        return "on"
    if key is False:
        return "off"
    return str(key)


def as_env_value(value):
    """Actions hands a step's env: to the shell as text. str() would spell a
    YAML boolean the Python way and send True where CI sends true."""
    if value is True:
        return "true"
    if value is False:
        return "false"
    if value is None:
        return ""
    return str(value)


# What this driver understands, as whitelists rather than a list of things to
# reject. A blacklist is silent about the construct nobody thought of, and
# being silent about a construct that changes what CI does is the exact defect
# this target was written to end -- it would just have moved one level up, from
# the step list to the workflow schema. Widening these means teaching the
# driver the construct first.
#
# Deliberately absent from JOB_KEYS: runs-on is read and ignored, because
# running on this machine instead of a runner is the point.
WORKFLOW_KEYS = ("name", "on", "jobs")
JOB_KEYS = ("name", "runs-on", "steps")
RUN_STEP_KEYS = ("name", "id", "run", "env")

problems = []

for key in parsed:
    if spelling(key) not in WORKFLOW_KEYS:
        problems.append(
            workflow + " sets '" + spelling(key) + "' at the top level, which"
            " make ci does not reproduce; it would apply to every step run here")

jobs = parsed.get("jobs") or {}
steps = []
for job_name in jobs:
    job = jobs[job_name] or {}
    for key in job:
        if spelling(key) not in JOB_KEYS:
            problems.append(
                "job '" + str(job_name) + "' sets '" + spelling(key) + "', which"
                " make ci does not reproduce; it would change what its steps do")
    position = 0
    for step in (job.get("steps") or []):
        position = position + 1
        if step.get("name"):
            key = str(step["name"])
        elif step.get("uses"):
            key = "uses:" + str(step["uses"])
        else:
            key = "<step at position " + str(position) + " in job " + str(job_name) + ">"
            problems.append(
                "job '" + str(job_name) + "' has a step with neither name: nor"
                " uses:, so CI_STEP_PLAN has no way to name it; give it a name:")
        steps.append((str(job_name), key, step))

plan = {}
for raw in os.environ.get("CI_STEP_PLAN", "").splitlines():
    line = raw.strip()
    if not line:
        continue
    parts = line.split("|", 3)
    if len(parts) != 4 or parts[0] not in ("run", "skip"):
        problems.append("unparseable CI_STEP_PLAN line: " + raw)
        continue
    disposition, job_name, key, reason = parts
    if disposition == "skip" and not reason.strip():
        problems.append("skip entry with no reason: " + key)
        continue
    if disposition == "run" and reason.strip():
        problems.append(
            "run entry carrying a reason, which nothing reads: " + key)
        continue
    if (job_name, key) in plan:
        problems.append("duplicate CI_STEP_PLAN entry: " + job_name + " / " + key)
        continue
    plan[(job_name, key)] = (disposition, reason)

seen = set()
for job_name, key, step in steps:
    if (job_name, key) in seen:
        problems.append(
            "two steps in job '" + job_name + "' share the key '" + key
            + "'; CI_STEP_PLAN cannot tell them apart")
    seen.add((job_name, key))
    if (job_name, key) not in plan:
        problems.append(
            workflow + " runs '" + key + "' in job '" + job_name
            + "' and CI_STEP_PLAN does not mention it")
for job_name, key in sorted(plan):
    if (job_name, key) not in seen:
        problems.append(
            "CI_STEP_PLAN still names '" + key + "' in job '" + job_name
            + "', which " + workflow + " no longer has")
for job_name in sorted(set(spelling(j) for j in jobs) - set(j for j, k in plan)):
    problems.append(
        workflow + " has a job '" + job_name + "' that CI_STEP_PLAN never mentions")

for job_name, key, step in steps:
    if plan.get((job_name, key), ("", ""))[0] != "run":
        continue
    for step_key in step:
        if spelling(step_key) == "if":
            problems.append(
                "'" + key + "' is conditional in CI (if:) and CI_STEP_PLAN runs"
                " it unconditionally; decide which and say so")
        elif spelling(step_key) not in RUN_STEP_KEYS:
            problems.append(
                "'" + key + "' is marked run and sets '" + spelling(step_key)
                + "', which make ci does not reproduce")
    body = step.get("run")
    if body is None:
        problems.append(
            "'" + key + "' is marked run, but it has no run: body to run")
    # The literal below is a GitHub expression, written with make's escape.
    # It is checked in the body and in every env: value, because env: is where
    # expressions and secrets actually live -- guarding only the body would
    # leave the likelier of the two sites silent.
    elif "$${{" in body:
        problems.append(
            "'" + key + "' is marked run, but its body holds a GitHub expression"
            " that only the runner can expand")
    for name in (step.get("env") or {}):
        if "$${{" in spelling(name) or "$${{" in as_env_value(step["env"][name]):
            problems.append(
                "'" + key + "' is marked run and its env: holds a GitHub"
                " expression that only the runner can expand")

if problems:
    sys.stderr.write(
        "make ci: the Makefile and " + workflow + " disagree about what CI runs.\n")
    for problem in problems:
        sys.stderr.write("  - " + problem + "\n")
    sys.stderr.write(
        "\nNothing was run. Give every step above a CI_STEP_PLAN line in the\n"
        "Makefile: 'run' to run CI's own command here, or 'skip' plus the reason\n"
        "it cannot run outside a runner. A construct this driver does not\n"
        "understand has to be taught to it, not skipped past.\n")
    sys.exit(1)

for job_name, key, step in steps:
    disposition, reason = plan[(job_name, key)]
    if disposition == "skip":
        print("--- skip  " + key + "  (" + reason + ")")
        sys.stdout.flush()
        continue
    print("==> " + key)
    sys.stdout.flush()
    env = dict(os.environ)
    # A CI step runs with no make anywhere above it. Handing it MAKELEVEL and
    # MAKEFLAGS would make every step a recursive make invocation, which is a
    # difference from CI introduced by this target and by nothing in ci.yml --
    # and a real one, not cosmetic: any step that reaches `make` then inherits
    # sub-make behaviour, starting with the "Entering directory" banner GNU make
    # prints only when MAKELEVEL is set. MAKE_TERMOUT and MAKE_TERMERR are set
    # only when make's own output is a terminal, so leaving them in would make a
    # step's environment depend on whether a human was watching. The CI_* names
    # are this target's own plumbing and go for the same reason. GOWORK
    # deliberately stays: see the comment on its declaration for why local runs
    # need it and the runner does not.
    for name in ("MAKELEVEL", "MAKEFLAGS", "MFLAGS",
                 "MAKE_TERMOUT", "MAKE_TERMERR",
                 "CI_WORKFLOW", "CI_STEP_PLAN", "CI_DERIVE"):
        env.pop(name, None)
    for name in (step.get("env") or {}):
        env[spelling(name)] = as_env_value(step["env"][name])
    # GitHub's default shell for a run: step that names no `shell:` is
    # `bash -e {0}` -- note no pipefail, which is why the cross-compile step
    # sets its own `set -euo pipefail`. Matching that exactly matters in the
    # cheap direction: adding pipefail here would redden locally on a pipeline
    # CI is happy with, and a target people rerun past is not a gate. A step
    # that does name a `shell:` cannot reach here -- RUN_STEP_KEYS rejects it.
    code = subprocess.call(
        ["bash", "--noprofile", "--norc", "-e", "-c", step["run"]], env=env)
    if code != 0:
        sys.stderr.write("make ci: FAILED at '" + key + "' (job " + job_name + ")\n")
        sys.exit(code if code > 0 else 1)

print("make ci: every step in " + workflow + " either ran here or is a declared skip")
endef

# Target-specific, so the driver source and the plan sit in the environment of
# this target's recipe and nothing else. Exported at file scope they would ride
# along in every `go`, `pnpm` and `bash` this Makefile ever starts.
ci: export CI_STEP_PLAN = $(CI_STEP_PLAN_BODY)
ci: export CI_DERIVE = $(CI_DERIVE_BODY)
ci:
	@python3 -c "$$CI_DERIVE"

release:
	bash scripts/release.sh

clean:
	rm -rf $(BINARY) dist/ release/
