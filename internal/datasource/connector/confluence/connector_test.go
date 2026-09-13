package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

type testHandler struct {
	items     []types.FetchedItem
	cursor    *types.SyncCursor
	committed bool
	emitErr   error
}

func (h *testHandler) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return h.emitErr
}
func (h *testHandler) Checkpoint(_ context.Context, cursor *types.SyncCursor) error {
	h.cursor = cursor
	return nil
}
func (h *testHandler) ItemCommitted(context.Context, *types.FetchedItem) (bool, error) {
	return h.committed, nil
}

func allowTestServer(t *testing.T) {
	t.Helper()
	utils.ResetSSRFWhitelistForTest()
	utils.SetSSRFWhitelistFromRaw("127.0.0.1")
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
}

func fixture(id, title string, version int) page {
	var p page
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"id":%q,"type":"page","status":"current","title":%q,"space":{"key":"OPS"},"version":{"number":%d},"body":{"storage":{"value":"<p>Published content</p>"}},"_links":{"webui":"/pages/viewpage.action?pageId=%s"}}`, id, title, version, id)), &p)
	return p
}

func TestStreamingRetriesUntilCommittedAndRefreshesEdits(t *testing.T) {
	allowTestServer(t)
	p := fixture("12", "Guide", 1)
	pageReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("PAT authentication missing")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/confluence/rest/api/content/search":
			if !strings.Contains(r.URL.Query().Get("cql"), `label="approved"`) {
				t.Error("publication label missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []page{p}})
		case "/confluence/rest/api/content/12":
			pageReads++
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL + "/confluence", "api_token": "test-token"}, ResourceIDs: []string{"space:OPS"}, Settings: map[string]any{"label": "approved", "attachments": false}}
	c := NewConnector()
	h := &testHandler{}
	cursor, err := c.FetchStream(context.Background(), raw, nil, h)
	if err != nil || len(h.items) != 1 {
		t.Fatalf("first submission: items=%d err=%v", len(h.items), err)
	}
	if !strings.HasPrefix(h.items[0].FileName, "confluence-12-") || h.items[0].Metadata["source_version"] != "1" {
		t.Fatal("missing identity/version")
	}
	h = &testHandler{committed: true}
	cursor, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 1 {
		t.Fatalf("uncommitted revision must retry: %v", err)
	}
	h = &testHandler{committed: true}
	cursor, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 0 || pageReads != 2 {
		t.Fatalf("unchanged revision must skip body fetch: reads=%d err=%v", pageReads, err)
	}
	p.Version.Number = 2
	p.Title = "Renamed Guide"
	h = &testHandler{committed: true}
	_, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 1 || h.items[0].ExternalID != "12" || h.items[0].Metadata["source_version"] != "2" {
		t.Fatalf("edit/rename did not refresh: %v", err)
	}
}

func TestPaginationCheckpointAndNoImplicitDeletion(t *testing.T) {
	allowTestServer(t)
	pages := []page{fixture("12", "First", 1), fixture("13", "Second", 1)}
	failSecond := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/content/search":
			start := 0
			if r.URL.Query().Get("start") == "1" {
				start = 1
			}
			result := map[string]any{"results": []page{pages[start]}}
			if start == 0 && len(pages) > 1 {
				result["_links"] = map[string]string{"next": "https://untrusted.invalid/wrong-path?start=1"}
			}
			_ = json.NewEncoder(w).Encode(result)
		case "/rest/api/content/12":
			_ = json.NewEncoder(w).Encode(pages[0])
		case "/rest/api/content/13":
			if failSecond {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(pages[1])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL, "api_token": "test-token"}, ResourceIDs: []string{"space:OPS"}, Settings: map[string]any{"attachments": false}}
	c := NewConnector()
	h := &testHandler{committed: true}
	cursor, err := c.FetchStream(context.Background(), raw, nil, h)
	if err != nil || len(h.items) != 2 {
		t.Fatalf("server-capped pagination lost pages: %v", err)
	}
	failSecond = true
	h = &testHandler{committed: true}
	_, err = c.FetchStream(context.Background(), raw, nil, h)
	if !errors.Is(err, datasource.ErrInvalidCredentials) || h.cursor == nil || len(h.items) != 1 {
		t.Fatalf("failed page advanced checkpoint: %v", err)
	}
	failSecond = false
	pages = pages[:1]
	h = &testHandler{committed: true}
	_, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 0 {
		t.Fatalf("missing pages must not emit deletion: %v", err)
	}
}

func TestFailedEmitDoesNotCommitRevision(t *testing.T) {
	allowTestServer(t)
	p := fixture("12", "Guide", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/search") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []page{p}})
		} else {
			_ = json.NewEncoder(w).Encode(p)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL, "api_token": "test-token"}, ResourceIDs: []string{"page:12"}, Settings: map[string]any{"attachments": false}}
	h := &testHandler{committed: true, emitErr: errors.New("index unavailable")}
	_, err := NewConnector().FetchStream(context.Background(), raw, nil, h)
	if err == nil || h.cursor != nil {
		t.Fatal("failed ingest committed its revision")
	}
}

func TestClientAuthAndRejectsHTMLOrRedirect(t *testing.T) {
	allowTestServer(t)
	mode := "basic"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "basic":
			user, password, ok := r.BasicAuth()
			if !ok || user != "employee@example.com" || password != "test-token" {
				t.Error("Basic authentication missing")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[]}`))
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>login</html>"))
		case "redirect":
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL, "api_token": "test-token", "username": "employee@example.com"}}
	if err := NewConnector().Validate(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	for _, next := range []string{"html", "redirect"} {
		mode = next
		if err := NewConnector().Validate(context.Background(), raw); err == nil {
			t.Fatalf("accepted %s response", mode)
		}
	}
	for _, base := range []string{"", "file:///tmp/doc", "https://user:password@example.com", "https://example.com?token=secret"} {
		raw.Credentials["base_url"] = base
		if _, err := parseConfig(raw); err == nil {
			t.Fatalf("accepted invalid base URL %q", base)
		}
	}
}

func TestMarkdownPreservesCodeTablesAndMarksUnsupportedContent(t *testing.T) {
	p := fixture("12", "Guide", 1)
	p.Body.Storage.Value = `<h2>Setup</h2><table><tr><th>Name</th><th>Value</th></tr><tr><td>port</td><td>8080</td></tr></table><ac:structured-macro ac:name="code"><ac:parameter ac:name="language">go</ac:parameter><ac:plain-text-body><![CDATA[if x < 3 { fmt.Println("hello") }]]></ac:plain-text-body></ac:structured-macro><ac:link><ri:page ri:content-id="13" ri:content-title="Other" /></ac:link><ac:structured-macro ac:name="jira" /><ac:image><ri:attachment ri:filename="diagram.png" /></ac:image>`
	text, warnings, err := markdown(p, "https://confluence.example.com/confluence")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"## Setup", "| Name", "8080", "if x < 3", `fmt.Println("hello")`, "pageId=13", "Confluence macro: jira", "diagram.png"} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in:\n%s", expected, text)
		}
	}
	if len(warnings) != 2 {
		t.Fatalf("expected macro/image warnings, got %v", warnings)
	}
	if safeFileName("船舶/操作:*?") != "船舶-操作---" {
		t.Fatal("unsafe filename conversion")
	}
}

func TestConfluenceDynamicMacrosRefreshWithoutPageVersionChange(t *testing.T) {
	allowTestServer(t)
	p := fixture("12", "Dashboard", 1)
	p.Body.Storage.Value = `<ac:structured-macro ac:name="jira"></ac:structured-macro>`
	p.Body.ExportView.Value = "<h2>Issues</h2><ul><li>OPS-123 Open</li></ul>"
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/search") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []page{p}})
		} else {
			reads++
			if !strings.Contains(r.URL.Query().Get("expand"), "body.export_view") {
				t.Error("rendered macro output not requested")
			}
			_ = json.NewEncoder(w).Encode(p)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL, "api_token": "test-token"}, ResourceIDs: []string{"page:12"}, Settings: map[string]any{"attachments": false}}
	c := NewConnector()
	h := &testHandler{committed: true}
	cursor, err := c.FetchStream(context.Background(), raw, nil, h)
	if err != nil || len(h.items) != 1 || !strings.Contains(string(h.items[0].Content), "OPS-123 Open") {
		t.Fatalf("macro output missing: %v", err)
	}
	h = &testHandler{committed: true}
	cursor, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 0 || reads != 2 {
		t.Fatal("unchanged rendered macro output reindexed or not checked")
	}
	p.Body.ExportView.Value = "<h2>Issues</h2><ul><li>OPS-123 Closed</li></ul>"
	h = &testHandler{committed: true}
	cursor, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 1 || !strings.Contains(string(h.items[0].Content), "OPS-123 Closed") {
		t.Fatal("dynamic macro change missed without page edit")
	}
	p.Body.ExportView.Value = `<div class="macro-error">Unable to render</div>`
	h = &testHandler{committed: true}
	if _, err := c.FetchStream(context.Background(), raw, cursor, h); err == nil || len(h.items) != 0 {
		t.Fatal("rendering error was silently imported")
	}
	p.Body.ExportView.Value = ""
	if _, err := c.FetchStream(context.Background(), raw, cursor, h); err == nil {
		t.Fatal("unrendered dynamic macro was silently imported")
	}
}
