package skill

import (
	_ "embed"
	"fmt"

	"github.com/BurntSushi/toml"
)

//go:embed skills.toml
var embeddedBlessedListTOML []byte

// EmbeddedBlessedList returns the blessed list compiled into the binary
// at build time from internal/skill/skills.toml. Used as the deterministic
// default when the user passes neither --blessed-list nor a local file.
//
// v0.1 ships with an empty list — entries are added per-PR as skills mature.
func EmbeddedBlessedList() (*BlessedList, error) {
	var l BlessedList
	if err := toml.Unmarshal(embeddedBlessedListTOML, &l); err != nil {
		return nil, fmt.Errorf("skill: parse embedded blessed list: %w", err)
	}
	if l.Skills == nil {
		l.Skills = map[string]BlessedEntry{}
	}
	return &l, nil
}
