package wire

import (
	"encoding/json"
	"time"
)

// WorkProject is the curated project descriptor for GET /api/v1/work/projects.
type WorkProject struct {
	Name string `json:"name"`
}

// WorkReadyItem is a work item in the items/needsHumanSession/unclassified
// segments of WorkQueue.
type WorkReadyItem struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	WIType      *string `json:"wiType,omitempty"`
	Priority    string  `json:"priority"`
	Goal        string  `json:"goal"`
	UnblockedAt *string `json:"unblockedAt,omitempty"`
	CreatedAt   string  `json:"createdAt,omitempty"`
}

// WorkRunningItem is a work item in the running/staleRunning segment of
// WorkQueue.
type WorkRunningItem struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Goal         string `json:"goal"`
	OwnerDisplay string `json:"ownerDisplay"`
	LastActiveAt string `json:"lastActiveAt"`
}

// WorkStalledItem is a work item in the stalled segment of WorkQueue.
type WorkStalledItem struct {
	ID               string `json:"id"`
	Slug             string `json:"slug"`
	StallReason      string `json:"stallReason"`
	StalledSince     string `json:"stalledSince"`
	LastActorDisplay string `json:"lastActorDisplay"`
}

// WorkPausedItem is a work item in the paused segment of WorkQueue.
type WorkPausedItem struct {
	ID               string  `json:"id"`
	Slug             string  `json:"slug"`
	PausedSince      string  `json:"pausedSince"`
	LastActorDisplay string  `json:"lastActorDisplay"`
	PauseReason      *string `json:"pauseReason,omitempty"`
}

// WorkQueue is the curated LCRS ready-queue response for
// GET /api/v1/work/queue.
type WorkQueue struct {
	Items             []WorkReadyItem   `json:"items"`
	Running           []WorkRunningItem `json:"running"`
	Stalled           []WorkStalledItem `json:"stalled"`
	Paused            []WorkPausedItem  `json:"paused"`
	NeedsHumanSession []WorkReadyItem   `json:"needsHumanSession"`
	Unclassified      []WorkReadyItem   `json:"unclassified"`
	StaleRunning      []WorkRunningItem `json:"staleRunning,omitempty"`
}

// WorkItemDetail merges the whitelisted work_item fields with the current
// step-machine state, for GET /api/v1/work/items/{id}.
type WorkItemDetail struct {
	ID                string   `json:"id"`
	Slug              string   `json:"slug"`
	Goal              string   `json:"goal"`
	Status            string   `json:"status"`
	Priority          string   `json:"priority"`
	WIType            *string  `json:"wiType,omitempty"`
	Labels            []string `json:"labels"`
	Content           *string  `json:"content,omitempty"`
	CurrentStep       *string  `json:"currentStep,omitempty"`
	CurrentStepStatus string   `json:"currentStepStatus"`
}

// WorkEvent mirrors one agent_events row for
// GET /api/v1/work/items/{id}/events.
type WorkEvent struct {
	Ts      time.Time       `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WorkEvents is the paginated event page returned by
// GET /api/v1/work/items/{id}/events.
type WorkEvents struct {
	Events     []WorkEvent `json:"events"`
	NextCursor *string     `json:"nextCursor,omitempty"`
}
