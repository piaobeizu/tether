// Package scenario resolves and parses polyforge-coding scenario step-graph
// files (`.repo/<repo>/<wiType>.<project>.md` / `.repo/<repo>/<wiType>.md`)
// into an ordered step DAG, mirroring the step-id semantics enforced by
// `.repo/polyforge-coding/.ci/check_step_ids.py` (tether#20 Task 4).
package scenario

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// StepNode is a single `## Step: <id>` node in a scenario graph, with the
// ids of the steps it depends on (explicit previous_steps references, or a
// sequential fallback when a non-first step has no explicit reference).
type StepNode struct {
	ID   string
	Prev []string
}

// StepGraph is the ordered set of step nodes parsed from one scenario md
// file.
type StepGraph struct {
	Nodes []StepNode
}

// identRe guards ResolveScenarioFile's wiType/project inputs against path
// traversal — both must be a bare identifier, never a path fragment.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ResolveScenarioFile locates the scenario graph md file for wiType/project
// under workspaceRoot/.repo. For each directory entry d under .repo it
// checks, in order, "<d>/<wiType>.<project>.md" then "<d>/<wiType>.md",
// returning the first that exists (as an absolute path). Returns ("", false)
// if none is found, if .repo doesn't exist, or if wiType/project fail the
// identifier guard.
func ResolveScenarioFile(workspaceRoot, wiType, project string) (string, bool) {
	if !identRe.MatchString(wiType) || !identRe.MatchString(project) {
		return "", false
	}

	repoDir := filepath.Join(workspaceRoot, ".repo")
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return "", false
	}

	for _, e := range entries {
		candidates := []string{
			filepath.Join(repoDir, e.Name(), wiType+"."+project+".md"),
			filepath.Join(repoDir, e.Name(), wiType+".md"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err != nil {
				continue
			}
			abs, err := filepath.Abs(c)
			if err != nil {
				return c, true
			}
			return abs, true
		}
	}
	return "", false
}

// stepHeadingRe matches a "## Step: <id>" heading line, capturing the step
// id. Mirrors _STEP_HEADING_RE in check_step_ids.py.
var stepHeadingRe = regexp.MustCompile(`(?m)^## Step:\s*(\w+)`)

// includeRe matches an "@include: <relpath>" directive within a step body.
var includeRe = regexp.MustCompile(`@include:\s*(\S+)`)

// refRe matches a previous_steps["id"] or previous_steps.id reference.
// Mirrors _REF_RE in check_step_ids.py.
var refRe = regexp.MustCompile(`previous_steps(?:\[\s*["']([A-Za-z0-9_]+)["']\s*\]|\.([A-Za-z0-9_]+))`)

// ParseStepGraph reads mdPath, splits it into "## Step: <id>" sections, and
// resolves each step's Prev edges:
//   - explicit previous_steps references found in the step's own body or in
//     any single-level @include'd file it names (only referencing a known
//     step id counts — nested @includes are not followed);
//   - a sequential fallback (Prev = [the immediately preceding step in file
//     order]) for any non-first step that ends up with no explicit
//     reference, so the progress backbone stays connected.
//
// A missing mdPath is a graceful degrade, not an error: ParseStepGraph
// returns (nil, nil) rather than an error in that case.
func ParseStepGraph(mdPath string) (*StepGraph, error) {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	text := string(data)

	locs := stepHeadingRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return &StepGraph{}, nil
	}

	type rawStep struct {
		id   string
		body string
	}
	raws := make([]rawStep, len(locs))
	knownIDs := make(map[string]bool, len(locs))
	for i, loc := range locs {
		id := text[loc[2]:loc[3]]
		bodyStart := loc[1]
		bodyEnd := len(text)
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		}
		raws[i] = rawStep{id: id, body: text[bodyStart:bodyEnd]}
		knownIDs[id] = true
	}

	dir := filepath.Dir(mdPath)
	nodes := make([]StepNode, len(raws))
	for i, rs := range raws {
		combined := rs.body
		for _, m := range includeRe.FindAllStringSubmatch(rs.body, -1) {
			rel := m[1]
			incPath := filepath.Join(dir, rel)
			relCheck, err := filepath.Rel(dir, incPath)
			if err != nil || strings.HasPrefix(relCheck, "..") || filepath.IsAbs(rel) {
				continue
			}
			f, err := os.Open(incPath)
			if err != nil {
				continue
			}
			incData, err := io.ReadAll(io.LimitReader(f, 1<<20))
			f.Close()
			if err != nil {
				continue
			}
			combined += "\n" + string(incData)
		}

		var prev []string
		seen := make(map[string]bool)
		for _, m := range refRe.FindAllStringSubmatch(combined, -1) {
			sid := m[1]
			if sid == "" {
				sid = m[2]
			}
			if sid == rs.id || !knownIDs[sid] || seen[sid] {
				continue
			}
			seen[sid] = true
			prev = append(prev, sid)
		}

		if len(prev) == 0 && i > 0 {
			prev = []string{raws[i-1].id}
		}

		nodes[i] = StepNode{ID: rs.id, Prev: prev}
	}

	return &StepGraph{Nodes: nodes}, nil
}
