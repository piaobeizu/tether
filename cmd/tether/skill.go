package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/piaobeizu/tether/internal/skill"
)

// skillCmd dispatches `tether skill <verb> ...`.
func skillCmd(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "tether skill: missing verb (install|list|remove|info)")
		return 2
	}
	switch args[0] {
	case "install":
		return skillInstall(args[1:])
	case "list":
		return skillList(args[1:])
	case "remove", "uninstall":
		return skillRemove(args[1:])
	case "info", "show":
		return skillInfo(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tether skill: unknown verb %q\n", args[0])
		return 2
	}
}

func skillInstall(args []string) int {
	fs := flag.NewFlagSet("tether skill install", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite existing installation")
	listPath := fs.String("blessed-list", "", "path to skills.toml (default: ./skills.toml if present)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: tether skill install <name|git-url> [--force] [--blessed-list path]")
		return 2
	}
	arg := fs.Arg(0)

	pool, err := skill.DefaultPool()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	list, err := loadBlessedListWithFallback(*listPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	url, name, err := skill.ResolveSource(arg, list)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ref := ""
	if list != nil {
		if entry, ok := list.Skills[name]; ok {
			ref = entry.Ref
		}
	}
	ctx := context.Background()
	m, err := pool.Install(ctx, url, name, skill.InstallOptions{Force: *force, Ref: ref})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("installed: %s\n", filepath.Join(pool.Root, name))
	if m != nil {
		fmt.Printf("  name:    %s\n", m.Skill.Name)
		fmt.Printf("  version: %s\n", m.Skill.Version)
		if m.Skill.Description != "" {
			fmt.Printf("  desc:    %s\n", m.Skill.Description)
		}
	} else {
		fmt.Println("  (no tether.toml — installed as plain cc plugin)")
	}
	return 0
}

func skillList(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: tether skill list")
		return 2
	}
	pool, err := skill.DefaultPool()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	names, err := pool.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(names) == 0 {
		fmt.Printf("(no skills installed in %s)\n", pool.Root)
		return 0
	}
	for _, n := range names {
		m, _ := pool.Info(n)
		if m != nil {
			fmt.Printf("%-24s %s\n", n, m.Skill.Version)
		} else {
			fmt.Printf("%-24s (no tether.toml)\n", n)
		}
	}
	return 0
}

func skillRemove(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tether skill remove <name>")
		return 2
	}
	pool, err := skill.DefaultPool()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !pool.Has(args[0]) {
		fmt.Fprintf(os.Stderr, "tether skill: %q is not installed\n", args[0])
		return 1
	}
	if err := pool.Remove(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("removed: %s\n", args[0])
	return 0
}

func skillInfo(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tether skill info <name>")
		return 2
	}
	pool, err := skill.DefaultPool()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	m, err := pool.Info(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("name:    %s\n", args[0])
	fmt.Printf("path:    %s\n", pool.SkillDir(args[0]))
	if m == nil {
		fmt.Println("manifest: (no tether.toml — plain cc plugin)")
		return 0
	}
	fmt.Printf("version: %s\n", m.Skill.Version)
	if m.Skill.Description != "" {
		fmt.Printf("desc:    %s\n", m.Skill.Description)
	}
	if m.Skill.Agents.Primary != "" {
		fmt.Printf("agent:   %s (tested: %v)\n", m.Skill.Agents.Primary, m.Skill.Agents.Tested)
	}
	if len(m.Tether.FencedBlocks.Emits) > 0 {
		fmt.Printf("emits:   %v\n", m.Tether.FencedBlocks.Emits)
	}
	if m.Tether.State.WorkspaceDir != "" {
		fmt.Printf("state:   %s (lock=%t, gitignore=%t)\n",
			m.Tether.State.WorkspaceDir,
			m.Tether.State.FileLock,
			m.Tether.State.GitignoreDefault)
	}
	return 0
}

// loadBlessedListWithFallback resolves the blessed list source in priority order:
//  1. --blessed-list <path> when explicitly passed
//  2. ./skills.toml next to cwd (developer override)
//  3. embedded copy compiled into the binary from internal/skill/skills.toml
func loadBlessedListWithFallback(explicit string) (*skill.BlessedList, error) {
	if explicit != "" {
		return skill.LoadBlessedList(explicit)
	}
	if _, err := os.Stat(skill.BlessedListPath); err == nil {
		return skill.LoadBlessedList(skill.BlessedListPath)
	}
	return skill.EmbeddedBlessedList()
}
