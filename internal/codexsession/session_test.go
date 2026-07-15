package codexsession

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dpemmons/intercom/internal/appserver"
)

type fakeClient struct {
	listResponses []appserver.ThreadListResponse
	listErr       error
	listCalls     []appserver.ThreadListParams
	readResponse  appserver.ThreadReadResponse
	readErr       error
	readCalls     []appserver.ThreadReadParams
}

func (f *fakeClient) ThreadList(_ context.Context, params appserver.ThreadListParams) (appserver.ThreadListResponse, error) {
	f.listCalls = append(f.listCalls, params)
	if f.listErr != nil {
		return appserver.ThreadListResponse{}, f.listErr
	}
	if len(f.listResponses) == 0 {
		return appserver.ThreadListResponse{}, errors.New("unexpected thread/list call")
	}
	response := f.listResponses[0]
	f.listResponses = f.listResponses[1:]
	return response, nil
}

func (f *fakeClient) ThreadRead(_ context.Context, params appserver.ThreadReadParams) (appserver.ThreadReadResponse, error) {
	f.readCalls = append(f.readCalls, params)
	return f.readResponse, f.readErr
}

func i64ptr(value int64) *int64 { return &value }

func thread(id, cwd, source string, recency int64) appserver.Thread {
	return appserver.Thread{
		ID:        id,
		CWD:       cwd,
		Source:    json.RawMessage(`"` + source + `"`),
		Preview:   "preview " + id,
		UpdatedAt: recency,
		RecencyAt: i64ptr(recency),
		Status:    appserver.ThreadStatus{Type: appserver.ThreadStatusNotLoaded},
	}
}

func TestListPaginatesFiltersDeduplicatesAndSorts(t *testing.T) {
	next := "page-2"
	wrongSource := thread("exec", "/project", "exec", 500)
	ephemeral := thread("ephemeral", "/project", "cli", 400)
	ephemeral.Ephemeral = true
	active := thread("active", "/project", "cli", 450)
	active.Status.Type = appserver.ThreadStatusActive
	parentID := "parent"
	child := thread("child", "/project", "cli", 425)
	child.ParentThreadID = &parentID
	client := &fakeClient{listResponses: []appserver.ThreadListResponse{
		{
			Data: []appserver.Thread{
				thread("older", "/project", "cli", 100),
				wrongSource,
				ephemeral,
				active,
				child,
				thread("other-cwd", "/other", "vscode", 600),
			},
			NextCursor: &next,
		},
		{Data: []appserver.Thread{
			thread("newer", "/project", "vscode", 300),
			thread("older", "/project", "cli", 100),
		}},
	}}

	candidates, err := List(context.Background(), client, Options{CWD: "/project", PageSize: 7})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, candidate := range candidates {
		ids = append(ids, candidate.Thread.ID)
	}
	if want := []string{"newer", "older"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("candidate ids = %v, want %v", ids, want)
	}
	if len(client.listCalls) != 2 {
		t.Fatalf("thread/list calls = %d, want 2", len(client.listCalls))
	}
	first := client.listCalls[0]
	if first.Cursor != nil || first.Limit == nil || *first.Limit != 7 {
		t.Fatalf("first pagination params = %+v", first)
	}
	if first.SortKey == nil || *first.SortKey != appserver.ThreadSortRecencyAt || first.SortDirection == nil || *first.SortDirection != appserver.SortDescending {
		t.Fatalf("sort params = %+v", first)
	}
	if first.Archived == nil || *first.Archived || first.CWD != "/project" {
		t.Fatalf("filter params = %+v", first)
	}
	if want := []appserver.ThreadSourceKind{appserver.ThreadSourceCLI, appserver.ThreadSourceVSCode}; !reflect.DeepEqual(first.SourceKinds, want) {
		t.Fatalf("source kinds = %v, want %v", first.SourceKinds, want)
	}
	if client.listCalls[1].Cursor == nil || *client.listCalls[1].Cursor != next {
		t.Fatalf("second cursor = %#v", client.listCalls[1].Cursor)
	}
}

func TestPagerSkipsFilteredEmptyPage(t *testing.T) {
	next := "next"
	client := &fakeClient{listResponses: []appserver.ThreadListResponse{
		{Data: []appserver.Thread{thread("wrong", "/elsewhere", "cli", 9)}, NextCursor: &next},
		{Data: []appserver.Thread{thread("right", "/project", "cli", 8)}},
	}}
	pager, err := NewPager(client, Options{CWD: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := pager.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if page.More || len(page.Candidates) != 1 || page.Candidates[0].Thread.ID != "right" {
		t.Fatalf("page = %+v", page)
	}
	exhausted, err := pager.Next(context.Background())
	if err != nil || exhausted.More || len(exhausted.Candidates) != 0 {
		t.Fatalf("exhausted page = %+v, err = %v", exhausted, err)
	}
}

func TestPagerRejectsRepeatedCursor(t *testing.T) {
	next := "same"
	client := &fakeClient{listResponses: []appserver.ThreadListResponse{
		{NextCursor: &next},
		{NextCursor: &next},
	}}
	pager, err := NewPager(client, Options{CWD: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pager.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "repeated pagination cursor") {
		t.Fatalf("Next error = %v", err)
	}
}

func TestAllCWDsOmitsServerCWDFilter(t *testing.T) {
	client := &fakeClient{listResponses: []appserver.ThreadListResponse{{
		Data: []appserver.Thread{thread("one", "/anywhere", "cli", 1)},
	}}}
	candidates, err := List(context.Background(), client, Options{AllCWDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || client.listCalls[0].CWD != nil {
		t.Fatalf("candidates = %+v, params = %+v", candidates, client.listCalls[0])
	}
}

func TestReadAppliesEligibilityRules(t *testing.T) {
	listed := thread("session-1", "/project", "vscode", 1)
	client := &fakeClient{
		listResponses: []appserver.ThreadListResponse{
			{Data: []appserver.Thread{listed}},
			{Data: []appserver.Thread{listed}},
			{Data: []appserver.Thread{listed}},
		},
		readResponse: appserver.ThreadReadResponse{Thread: listed},
	}
	candidate, err := Read(context.Background(), client, "session-1", Options{CWD: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Source != appserver.ThreadSourceVSCode || len(client.readCalls) != 1 || client.readCalls[0].IncludeTurns {
		t.Fatalf("candidate = %+v, calls = %+v", candidate, client.readCalls)
	}
	client.readResponse.Thread.Ephemeral = true
	if _, err := Read(context.Background(), client, "session-1", Options{CWD: "/project"}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ephemeral Read error = %v", err)
	}
	client.readResponse.Thread = thread("different", "/project", "cli", 1)
	if _, err := Read(context.Background(), client, "session-1", Options{CWD: "/project"}); err == nil || !strings.Contains(err.Error(), "returned id") {
		t.Fatalf("mismatched Read error = %v", err)
	}
}

func TestReadRejectsSessionAbsentFromNonArchivedList(t *testing.T) {
	client := &fakeClient{listResponses: []appserver.ThreadListResponse{{}}}
	if _, err := Read(context.Background(), client, "archived-or-missing", Options{CWD: "/project"}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("Read error = %v", err)
	}
	if len(client.readCalls) != 0 {
		t.Fatalf("thread/read called for ineligible session: %#v", client.readCalls)
	}
}

func TestValidateID(t *testing.T) {
	for _, valid := range []string{"019f6335-57f4-7282-ae34-57524fa67702", "future-id:value"} {
		if err := ValidateID(valid); err != nil {
			t.Errorf("ValidateID(%q) = %v", valid, err)
		}
	}
	for _, invalid := range []string{"", " id", "id ", "two ids", "id\nother", strings.Repeat("x", 257), string([]byte{0xff})} {
		if err := ValidateID(invalid); err == nil {
			t.Errorf("ValidateID(%q) succeeded", invalid)
		}
	}
}

func TestCandidateTitleAndRecencyFallbacks(t *testing.T) {
	name := "named"
	candidate := Candidate{Thread: appserver.Thread{Name: &name, Preview: "preview", UpdatedAt: 20, CreatedAt: 10}}
	if candidate.Title() != name || candidate.Recency().Unix() != 20 {
		t.Fatalf("candidate title/recency = %q/%d", candidate.Title(), candidate.Recency().Unix())
	}
	empty := "  "
	candidate.Thread.Name = &empty
	if candidate.Title() != "preview" {
		t.Fatalf("preview title = %q", candidate.Title())
	}
	candidate.Thread.Preview = ""
	if candidate.Title() != "(untitled)" {
		t.Fatalf("empty title = %q", candidate.Title())
	}
}

func TestOptionsRequireProjectUnlessAllCWDs(t *testing.T) {
	client := &fakeClient{}
	if _, err := NewPager(client, Options{}); err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("NewPager error = %v", err)
	}
	if _, err := NewPager(nil, Options{AllCWDs: true}); err == nil || !strings.Contains(err.Error(), "nil client") {
		t.Fatalf("nil-client NewPager error = %v", err)
	}
}

func TestListWrapsClientError(t *testing.T) {
	client := &fakeClient{listErr: errors.New("boom")}
	_, err := List(context.Background(), client, Options{CWD: "/project"})
	if err == nil || !strings.Contains(err.Error(), "list sessions: boom") {
		t.Fatalf("List error = %v", err)
	}
}

func TestReadWrapsClientError(t *testing.T) {
	client := &fakeClient{
		listResponses: []appserver.ThreadListResponse{{Data: []appserver.Thread{thread("id", "/project", "cli", 1)}}},
		readErr:       errors.New("missing"),
	}
	_, err := Read(context.Background(), client, "id", Options{CWD: "/project"})
	if err == nil || !strings.Contains(err.Error(), `read session "id": missing`) {
		t.Fatalf("Read error = %v", err)
	}
}
