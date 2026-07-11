package aihub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrForbidden is returned by getJSON (and any typed method built on it)
// when aihub responds with 403 Forbidden.
var ErrForbidden = errors.New("aihub: forbidden")

// Client is a minimal read-only HTTP client for the aihub API.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New constructs a Client for the given aihub base URL and API key.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// getJSON issues a GET request to baseURL+path with the
// "Authorization: Bearer <key>" header set. On a 2xx response it decodes
// the JSON body into out. A 403 response returns ErrForbidden (checkable
// via errors.Is). Any other non-2xx response returns a descriptive error
// that includes the HTTP status.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("aihub: build request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("aihub: request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("aihub: GET %s returned status %d", path, resp.StatusCode)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("aihub: decode response from %s: %w", path, err)
	}
	return nil
}

// ReadyQueue fetches the six-segment LCRS ready queue for a project.
// GET /v1/work_items/ready?project=<project>&max=<max>
func (c *Client) ReadyQueue(ctx context.Context, project string, max int) (*ReadyQueue, error) {
	q := url.Values{}
	q.Set("project", project)
	q.Set("max", strconv.Itoa(max))

	var out ReadyQueue
	if err := c.getJSON(ctx, "/v1/work_items/ready?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkItem fetches a single work item by id or slug.
// GET /v1/work_items/<id>
func (c *Client) WorkItem(ctx context.Context, id string) (*WorkItem, error) {
	var out WorkItem
	if err := c.getJSON(ctx, "/v1/work_items/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StepState fetches the current step-machine state for a work item.
// GET /v1/work_items/<id>/step
func (c *Client) StepState(ctx context.Context, id string) (*StepState, error) {
	var out StepState
	if err := c.getJSON(ctx, "/v1/work_items/"+url.PathEscape(id)+"/step", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Dependencies fetches the blocking/blocked-by dependency edges for a work
// item. GET /v1/work_items/<id>/dependencies
func (c *Client) Dependencies(ctx context.Context, id string) (*Dependencies, error) {
	var out Dependencies
	if err := c.getJSON(ctx, "/v1/work_items/"+url.PathEscape(id)+"/dependencies", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Events fetches a page of agent events for a work item.
// GET /v1/events?work_item_id=<id>&limit=<limit>&cursor=<cursor>
func (c *Client) Events(ctx context.Context, id string, limit int, cursor string) (*EventsResponse, error) {
	q := url.Values{}
	q.Set("work_item_id", id)
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}

	var out EventsResponse
	if err := c.getJSON(ctx, "/v1/events?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Projects fetches the list of accessible projects.
// GET /v1/projects
//
// aihub wraps the array as {"items": [...]}, not a bare JSON array; that
// wrapper is unpacked here so callers get the plain []Project the exported
// signature promises.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var wrapper struct {
		Items []Project `json:"items"`
	}
	if err := c.getJSON(ctx, "/v1/projects", &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Items, nil
}

// ListWorkItems fetches work items for a project, optionally filtered by
// status (e.g. []string{"wrapped","cancelled"} for the done/recent view).
// GET /v1/work_items?project=<project>&status=<csv>&limit=<limit>
func (c *Client) ListWorkItems(ctx context.Context, project string, statuses []string, limit int) (*WorkItemList, error) {
	q := url.Values{}
	q.Set("project", project)
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	var out WorkItemList
	if err := c.getJSON(ctx, "/v1/work_items?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReadyQueue mirrors aihub's GET /v1/work_items/ready response
// (internal/domain.ReadyQueue in aihub).
type ReadyQueue struct {
	Items             []ReadyItem   `json:"items"`
	Running           []RunningItem `json:"running"`
	Stalled           []StalledItem `json:"stalled"`
	Paused            []PausedItem  `json:"paused"`
	NeedsHumanSession []ReadyItem   `json:"needs_human_session"`
	Unclassified      []ReadyItem   `json:"unclassified"`
	StaleRunning      []RunningItem `json:"stale_running,omitempty"`
}

// ReadyItem is a work item in the items/needs_human_session/unclassified
// segments of ReadyQueue.
type ReadyItem struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	WIType      *string `json:"wi_type"`
	Priority    string  `json:"priority"`
	Goal        string  `json:"goal"`
	UnblockedAt *string `json:"unblocked_at,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
}

// RunningItem is a work item in the running (or stale_running) segment.
type RunningItem struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Goal         string `json:"goal"`
	OwnerDisplay string `json:"owner_display"`
	LastActiveAt string `json:"last_active_at"`
}

// StalledItem is a work item in the stalled segment.
type StalledItem struct {
	ID               string `json:"id"`
	Slug             string `json:"slug"`
	StallReason      string `json:"stall_reason"`
	StalledSince     string `json:"stalled_since"`
	LastActorDisplay string `json:"last_actor_display"`
}

// PausedItem is a work item in the paused segment.
type PausedItem struct {
	ID               string  `json:"id"`
	Slug             string  `json:"slug"`
	PausedSince      string  `json:"paused_since"`
	LastActorDisplay string  `json:"last_actor_display"`
	PauseReason      *string `json:"pause_reason,omitempty"`
}

// WorkItem mirrors the MVP-whitelisted fields of aihub's work_items row
// (internal/domain.WorkItem in aihub). Project is added for tether#20 Task 5
// (the /steps endpoint needs it to resolve the scenario graph file), mirroring
// the project field aihub already surfaces per-entry on DependencyEntry.
type WorkItem struct {
	ID       string   `json:"id"`
	Slug     string   `json:"slug"`
	Goal     string   `json:"goal"`
	Status   string   `json:"status"`
	Priority string   `json:"priority"`
	WIType   *string  `json:"wi_type"`
	Labels   []string `json:"labels"`
	Content  *string  `json:"content"`
	Project  string   `json:"project"`
}

// StepState mirrors the MVP-whitelisted fields of aihub's
// GET /v1/work_items/:id/step response (internal/server.StepState in aihub,
// which itself lives in the server package rather than domain).
type StepState struct {
	CurrentStep       *string `json:"current_step,omitempty"`
	CurrentStepStatus string  `json:"current_step_status"`
}

// Event mirrors the MVP-whitelisted fields of one agent_events row as
// returned by GET /v1/events (internal/domain.EventRow in aihub).
type Event struct {
	CreatedAt time.Time       `json:"created_at"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// EventsResponse mirrors aihub's GET /v1/events response
// (internal/domain.ListEventsResponse in aihub).
type EventsResponse struct {
	Events     []Event `json:"events"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// Project mirrors the MVP-whitelisted field of aihub's projects row
// (internal/domain.Project in aihub).
type Project struct {
	Name string `json:"name"`
}

// WorkItemSummary mirrors the whitelisted fields of one work_items row as
// returned in the GET /v1/work_items list — a subset of the full row, enough
// for the done/recent history view.
type WorkItemSummary struct {
	ID               string  `json:"id"`
	Slug             string  `json:"slug"`
	Goal             string  `json:"goal"`
	Status           string  `json:"status"`
	Priority         string  `json:"priority"`
	WIType           *string `json:"wi_type"`
	ClosedAt         *string `json:"closed_at"`
	UpdatedAt        string  `json:"updated_at,omitempty"`
	ParentWorkItemID *string `json:"parent_work_item_id"`
}

// WorkItemList mirrors aihub's GET /v1/work_items list response
// ({"items":[...],"next_cursor":...}).
type WorkItemList struct {
	Items      []WorkItemSummary `json:"items"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}

// DependencyEntry is one dependency edge as returned by aihub's
// GET /v1/work_items/:id/dependencies (internal/domain.DependencyEntry).
type DependencyEntry struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Note    string `json:"note"`
}

// Dependencies mirrors aihub's GET /v1/work_items/:id/dependencies response.
type Dependencies struct {
	Blocking  []DependencyEntry `json:"blocking"`
	BlockedBy []DependencyEntry `json:"blocked_by"`
}
