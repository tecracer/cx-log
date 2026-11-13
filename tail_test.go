package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/megaproaktiv/agentic-cx-designer-sdk-go/acxd"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// fakeLogs is a QueryLogs server holding an in-memory event list. It
// honors the absolute time filter, sort order, maxResults and nextToken
// (an offset), and records each request.
type fakeLogs struct {
	mu       sync.Mutex
	events   []acxd.LogEntry
	requests []map[string]any
	headers  http.Header
}

func (f *fakeLogs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/sdk/logs/query" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		TimeFilter struct {
			Absolute struct{ StartTimestamp, EndTimestamp time.Time }
		}
		SortOrder  string
		MaxResults int
		NextToken  string
	}
	var raw map[string]any
	body := new(bytes.Buffer)
	body.ReadFrom(r.Body)
	json.Unmarshal(body.Bytes(), &req)
	json.Unmarshal(body.Bytes(), &raw)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, raw)
	f.headers = r.Header.Clone()

	var hits []acxd.LogEntry
	for _, e := range f.events {
		if !e.EventTime.Before(req.TimeFilter.Absolute.StartTimestamp) && !e.EventTime.After(req.TimeFilter.Absolute.EndTimestamp) {
			hits = append(hits, e)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if req.SortOrder == "desc" {
			return hits[i].EventTime.After(hits[j].EventTime)
		}
		return hits[i].EventTime.Before(hits[j].EventTime)
	})
	off := 0
	if req.NextToken != "" {
		json.Unmarshal([]byte(req.NextToken), &off)
	}
	resp := acxd.QueryLogsResponse{QueryStatus: acxd.LogQueryStatus{ProgressPercentage: 100}, Items: []acxd.LogEntry{}}
	if off < len(hits) {
		end := min(off+req.MaxResults, len(hits))
		resp.Items = hits[off:end]
		if end < len(hits) {
			tok, _ := json.Marshal(end)
			s := string(tok)
			resp.NextToken = &s
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func ev(sec int, typ acxd.LogEventType, kv ...string) acxd.LogEntry {
	e := acxd.LogEntry{EventType: typ, EventTime: t0.Add(time.Duration(sec) * time.Second)}
	for i := 0; i+1 < len(kv); i += 2 {
		e.EventProperties = append(e.EventProperties, acxd.LogProperty{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

func newTestTailer(t *testing.T, f *fakeLogs, o tailOptions) (*tailer, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := acxd.New(acxd.WithEndpointURL(srv.URL), acxd.WithAPIKey("k"), acxd.WithWorkspaceID("ws"))
	out := &bytes.Buffer{}
	if o.PageSize == 0 {
		o.PageSize = 5
	}
	if o.Since == 0 {
		o.Since = time.Hour
	}
	o.Printer = &textPrinter{w: out}
	return newTailer(client, o), out
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

func TestLastNOldestFirstAcrossPages(t *testing.T) {
	f := &fakeLogs{}
	for i := 0; i < 12; i++ {
		f.events = append(f.events, ev(i, acxd.LogEventTypeNodeTraversal, "i", string(rune('a'+i))))
	}
	tl, out := newTestTailer(t, f, tailOptions{Lines: 7})
	tl.now = func() time.Time { return t0.Add(time.Minute) }

	if err := tl.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := lines(out.String())
	if len(got) != 7 || !strings.HasSuffix(got[0], "i=f") || !strings.HasSuffix(got[6], "i=l") {
		t.Fatalf("want events f..l oldest first, got:\n%s", out)
	}
	if len(f.requests) != 2 {
		t.Fatalf("want 2 pages (5+2), got %d requests", len(f.requests))
	}
	if f.headers.Get("x-api-key") != "k" || f.headers.Get("x-workspace-id") != "ws" {
		t.Fatalf("auth headers not sent: %v", f.headers)
	}
}

func TestEventTypeFilterAndSearchFilter(t *testing.T) {
	f := &fakeLogs{events: []acxd.LogEntry{
		ev(1, acxd.LogEventTypeError, "msg", "boom one"),
		ev(2, acxd.LogEventTypeNodeTraversal),
		ev(3, acxd.LogEventTypeModelError),
	}}
	conv := "c-1"
	tl, out := newTestTailer(t, f, tailOptions{
		Lines:      10,
		EventTypes: parseEventTypes("Error, ModelError"),
		Filter:     &acxd.LogSearchFilter{ConversationId: &conv},
	})
	tl.now = func() time.Time { return t0.Add(time.Minute) }
	if err := tl.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := lines(out.String())
	if len(got) != 2 || !strings.Contains(got[0], `msg="boom one"`) || !strings.Contains(got[1], "ModelError") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	sf, _ := f.requests[0]["searchFilter"].(map[string]any)
	if sf["conversationId"] != "c-1" {
		t.Fatalf("searchFilter not sent: %v", f.requests[0])
	}
}

func TestFollowPrintsNewEventsOnceIncludingLateOnes(t *testing.T) {
	f := &fakeLogs{events: []acxd.LogEntry{ev(0, acxd.LogEventTypeAgentStarted)}}
	tl, out := newTestTailer(t, f, tailOptions{Lines: 10, Follow: true, Interval: 10 * time.Second, Lag: time.Minute})

	clock := t0.Add(5 * time.Second)
	tl.now = func() time.Time { return clock }
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	tl.sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		clock = clock.Add(d)
		switch polls {
		case 1: // new event arrives
			f.mu.Lock()
			f.events = append(f.events, ev(8, acxd.LogEventTypeModelStart))
			f.mu.Unlock()
		case 2: // a late-ingested event, timestamped before the last poll's end
			f.mu.Lock()
			f.events = append(f.events, ev(12, acxd.LogEventTypeModelEnd))
			f.mu.Unlock()
		case 4:
			cancel()
			return ctx.Err()
		}
		return nil
	}

	if err := tl.Run(ctx); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	got := lines(out.String())
	want := []string{"AgentStarted", "ModelStart", "ModelEnd"}
	if len(got) != len(want) {
		t.Fatalf("want %d lines, got:\n%s", len(want), out)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Fatalf("line %d: want %s, got %q", i, w, got[i])
		}
	}
}

func TestRetriesThrottling(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"__type":"ThrottlingException","message":"slow down","retryAfterSeconds":2}`))
			return
		}
		w.Write([]byte(`{"queryStatus":{"progressPercentage":100,"cumulativeBytesScanned":0,"cumulativeBytesMetered":0},"items":[]}`))
	}))
	defer srv.Close()
	tl := newTailer(acxd.New(acxd.WithEndpointURL(srv.URL)), tailOptions{Lines: 1, Since: time.Hour, PageSize: 5, Printer: &textPrinter{w: &bytes.Buffer{}}})
	var waited time.Duration
	tl.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if err := tl.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || waited != 2*time.Second {
		t.Fatalf("calls=%d waited=%v", calls, waited)
	}
}

func TestLoadDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# c\nexport CXLOG_T_A=one\nCXLOG_T_B=\"two words\"\nCXLOG_T_C=keep # note\nCXLOG_T_D=fromfile\n"), 0o600)
	t.Setenv("CXLOG_T_D", "fromenv")
	for _, k := range []string{"CXLOG_T_A", "CXLOG_T_B", "CXLOG_T_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := loadDotEnv(p, true); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"CXLOG_T_A": "one", "CXLOG_T_B": "two words", "CXLOG_T_C": "keep", "CXLOG_T_D": "fromenv"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing"), false); err != nil {
		t.Errorf("missing optional file: %v", err)
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing"), true); err == nil {
		t.Error("missing explicit file: want error")
	}
}

func TestEnvFileFromArgs(t *testing.T) {
	cases := map[string][]string{
		".env":  {"-f"},
		"a.env": {"-f", "--env-file", "a.env"},
		"b.env": {"-env-file=b.env"},
	}
	for want, args := range cases {
		if got, _ := envFileFromArgs(args); got != want {
			t.Errorf("%v: got %q want %q", args, got, want)
		}
	}
}
