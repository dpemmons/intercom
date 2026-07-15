// Package codexsession discovers and selects materialized interactive Codex
// sessions through app-server.
package codexsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dpemmons/intercom/internal/appserver"
)

const DefaultPageSize uint32 = 50

var (
	ErrNoCandidates = errors.New("codexsession: no resumable sessions")
	ErrNotResumable = errors.New("codexsession: session is not resumable by this picker")
)

// Client is the read-only app-server surface used for session discovery.
type Client interface {
	ThreadList(context.Context, appserver.ThreadListParams) (appserver.ThreadListResponse, error)
	ThreadRead(context.Context, appserver.ThreadReadParams) (appserver.ThreadReadResponse, error)
}

// Options defines the sessions eligible for selection. By default, CWD is an
// exact project-directory filter. AllCWDs suppresses that filter.
type Options struct {
	CWD      string
	AllCWDs  bool
	PageSize uint32
}

func (o Options) validate() error {
	if !o.AllCWDs && o.CWD == "" {
		return errors.New("codexsession: project cwd is empty")
	}
	return nil
}

func (o Options) pageSize() uint32 {
	if o.PageSize == 0 {
		return DefaultPageSize
	}
	return o.PageSize
}

// Candidate is a non-archived, non-ephemeral CLI or VS Code thread that
// satisfies the requested cwd filter.
type Candidate struct {
	Thread appserver.Thread
	Source appserver.ThreadSourceKind
}

// Title returns the user-facing name when present, then the first-message
// preview, then a fixed placeholder.
func (c Candidate) Title() string {
	if c.Thread.Name != nil && strings.TrimSpace(*c.Thread.Name) != "" {
		return *c.Thread.Name
	}
	if strings.TrimSpace(c.Thread.Preview) != "" {
		return c.Thread.Preview
	}
	return "(untitled)"
}

// Recency returns the protocol recency timestamp, falling back to updated and
// created timestamps for older session metadata.
func (c Candidate) Recency() time.Time {
	seconds := c.Thread.UpdatedAt
	if c.Thread.RecencyAt != nil {
		seconds = *c.Thread.RecencyAt
	} else if seconds == 0 {
		seconds = c.Thread.CreatedAt
	}
	return time.Unix(seconds, 0)
}

// Pager follows opaque thread/list cursors. It requests newest-first CLI and
// VS Code sessions and defensively reapplies all eligibility filters to every
// returned page.
type Pager struct {
	client Client
	opts   Options
	cursor *string
	done   bool
	seen   map[string]struct{}
}

func NewPager(client Client, opts Options) (*Pager, error) {
	if client == nil {
		return nil, errors.New("codexsession: nil client")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Pager{client: client, opts: opts, seen: make(map[string]struct{})}, nil
}

type Page struct {
	Candidates []Candidate
	More       bool
}

// Next returns the next non-empty eligible page. An empty page with More false
// denotes exhaustion. A Pager must not be used concurrently.
func (p *Pager) Next(ctx context.Context) (Page, error) {
	if p.done {
		return Page{}, nil
	}
	for {
		params := p.params()
		response, err := p.client.ThreadList(ctx, params)
		if err != nil {
			return Page{}, fmt.Errorf("codexsession: list sessions: %w", err)
		}

		candidates := make([]Candidate, 0, len(response.Data))
		for _, thread := range response.Data {
			candidate, ok := makeCandidate(thread, p.opts)
			if ok {
				candidates = append(candidates, candidate)
			}
		}
		sortCandidates(candidates)

		more, err := p.advance(response.NextCursor)
		if err != nil {
			return Page{}, err
		}
		if len(candidates) != 0 || !more {
			return Page{Candidates: candidates, More: more}, nil
		}
	}
}

func (p *Pager) params() appserver.ThreadListParams {
	limit := p.opts.pageSize()
	sortKey := appserver.ThreadSortRecencyAt
	direction := appserver.SortDescending
	archived := false
	params := appserver.ThreadListParams{
		Cursor:        p.cursor,
		Limit:         &limit,
		SortKey:       &sortKey,
		SortDirection: &direction,
		SourceKinds: []appserver.ThreadSourceKind{
			appserver.ThreadSourceCLI,
			appserver.ThreadSourceVSCode,
		},
		Archived: &archived,
	}
	if !p.opts.AllCWDs {
		params.CWD = p.opts.CWD
	}
	return params
}

func (p *Pager) advance(next *string) (bool, error) {
	if next == nil {
		p.done = true
		p.cursor = nil
		return false, nil
	}
	if *next == "" {
		return false, errors.New("codexsession: app-server returned an empty pagination cursor")
	}
	if _, duplicate := p.seen[*next]; duplicate {
		return false, fmt.Errorf("codexsession: app-server repeated pagination cursor %q", *next)
	}
	p.seen[*next] = struct{}{}
	p.cursor = next
	return true, nil
}

// List collects all eligible pages and returns a globally newest-first,
// thread-ID-deduplicated slice.
func List(ctx context.Context, client Client, opts Options) ([]Candidate, error) {
	pager, err := NewPager(client, opts)
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	seenIDs := make(map[string]struct{})
	for {
		page, err := pager.Next(ctx)
		if err != nil {
			return nil, err
		}
		for _, candidate := range page.Candidates {
			if _, duplicate := seenIDs[candidate.Thread.ID]; duplicate {
				continue
			}
			seenIDs[candidate.Thread.ID] = struct{}{}
			candidates = append(candidates, candidate)
		}
		if !page.More {
			break
		}
	}
	sortCandidates(candidates)
	return candidates, nil
}

// Read resolves an explicit thread ID through the non-archived list, then
// refreshes it with thread/read. This applies the same source, cwd, ephemeral,
// and archive eligibility contract as interactive selection while retaining a
// current status and ancestry snapshot for adoption.
func Read(ctx context.Context, client Client, id string, opts Options) (Candidate, error) {
	if client == nil {
		return Candidate{}, errors.New("codexsession: nil client")
	}
	if err := opts.validate(); err != nil {
		return Candidate{}, err
	}
	if err := ValidateID(id); err != nil {
		return Candidate{}, err
	}
	candidates, err := List(ctx, client, opts)
	if err != nil {
		return Candidate{}, err
	}
	found := false
	for _, candidate := range candidates {
		if candidate.Thread.ID == id {
			found = true
			break
		}
	}
	if !found {
		return Candidate{}, fmt.Errorf("%w: %q", ErrNotResumable, id)
	}
	response, err := client.ThreadRead(ctx, appserver.ThreadReadParams{ThreadID: id})
	if err != nil {
		return Candidate{}, fmt.Errorf("codexsession: read session %q: %w", id, err)
	}
	if response.Thread.ID != id {
		return Candidate{}, fmt.Errorf("codexsession: thread/read returned id %q for requested session %q", response.Thread.ID, id)
	}
	candidate, ok := makeCandidate(response.Thread, opts)
	if !ok {
		return Candidate{}, fmt.Errorf("%w: %q", ErrNotResumable, id)
	}
	return candidate, nil
}

// ValidateID performs transport-safe validation without assuming that future
// Codex thread identifiers will remain UUIDs.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("codexsession: session id is empty")
	}
	if len(id) > 256 {
		return errors.New("codexsession: session id exceeds 256 bytes")
	}
	if !utf8.ValidString(id) {
		return errors.New("codexsession: session id is not valid UTF-8")
	}
	if strings.TrimSpace(id) != id {
		return errors.New("codexsession: session id has leading or trailing whitespace")
	}
	for _, r := range id {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return errors.New("codexsession: session id contains whitespace or a control character")
		}
	}
	return nil
}

func makeCandidate(thread appserver.Thread, opts Options) (Candidate, bool) {
	if ValidateID(thread.ID) != nil || thread.Ephemeral || thread.ParentThreadID != nil ||
		(!opts.AllCWDs && thread.CWD != opts.CWD) ||
		(thread.Status.Type != appserver.ThreadStatusIdle && thread.Status.Type != appserver.ThreadStatusNotLoaded) {
		return Candidate{}, false
	}
	var source string
	if err := json.Unmarshal(thread.Source, &source); err != nil {
		return Candidate{}, false
	}
	kind := appserver.ThreadSourceKind(source)
	if kind != appserver.ThreadSourceCLI && kind != appserver.ThreadSourceVSCode {
		return Candidate{}, false
	}
	return Candidate{Thread: thread, Source: kind}, true
}

func sortCandidates(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].Recency(), candidates[j].Recency()
		if !left.Equal(right) {
			return left.After(right)
		}
		return candidates[i].Thread.ID < candidates[j].Thread.ID
	})
}
