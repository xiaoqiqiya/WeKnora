package confluence

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/joho/godotenv"
)

// Opt-in, read-only real-site check. Never logs credentials or document bodies.
func TestConfluenceLiveRead(t *testing.T) {
	path := os.Getenv("CONFLUENCE_LIVE_ENV_FILE")
	if path == "" {
		t.Skip("set CONFLUENCE_LIVE_ENV_FILE and CONFLUENCE_LIVE_PAGE_ID for a real-site read check")
	}
	env, err := godotenv.Read(path)
	if err != nil {
		t.Fatal("cannot read live credential file")
	}
	base := strings.TrimRight(env["CONFLUENCE_URL"], "/")
	if base == "" {
		base = strings.TrimRight(env["ATLASSIAN_HOST"], "/")
	}
	site, err := url.Parse(base)
	if err != nil || site.Hostname() == "" {
		t.Fatal("invalid live site URL")
	}
	id := os.Getenv("CONFLUENCE_LIVE_PAGE_ID")
	if !regexp.MustCompile(`^\d+$`).MatchString(id) {
		t.Fatal("set a numeric CONFLUENCE_LIVE_PAGE_ID")
	}
	credentials := map[string]interface{}{"base_url": base}
	if env["ATLASSIAN_AUTH_METHOD"] == "pat" {
		credentials["api_token"] = env["ATLASSIAN_PAT"]
	} else {
		credentials["api_token"] = env["ATLASSIAN_API_TOKEN"]
		username := env["CONFLUENCE_USERNAME"]
		if username == "" {
			username = env["ATLASSIAN_EMAIL"]
		}
		credentials["username"] = username
	}
	// Permit only this explicitly selected site's host within this test process.
	utils.SetSSRFWhitelistFromRaw(site.Hostname())
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
	raw := &types.DataSourceConfig{Credentials: credentials, ResourceIDs: []string{"page:" + id}, Settings: map[string]interface{}{"attachments": true, "images": true}, MultimodalEnabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connector := NewConnector()
	if err := connector.Validate(ctx, raw); err != nil {
		t.Fatalf("live connection validation: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newClient(cfg).page(ctx, id)
	if err != nil {
		t.Fatalf("live page read: %v", err)
	}
	parentID := "space:" + p.Space.Key
	pathIDs := []string{}
	for _, ancestor := range p.Ancestors {
		pathIDs = append(pathIDs, ancestor.ID)
	}
	pathIDs = append(pathIDs, id)
	var selected types.Resource
	for _, pathID := range pathIDs {
		rows, err := connector.ListResources(ctx, raw, parentID)
		if err != nil {
			t.Fatalf("real resource listing: %v", err)
		}
		found := false
		for _, row := range rows {
			if row.ExternalID == "page:"+pathID {
				selected = row
				found = true
				t.Logf("tree_parent=%s page=%s has_children=%t", parentID, row.ExternalID, row.HasChildren)
			}
		}
		if !found {
			t.Fatalf("page %s is missing under resource %s", pathID, parentID)
		}
		parentID = "page:" + pathID
	}
	children, err := connector.ListResources(ctx, raw, "page:"+id)
	if err != nil {
		t.Fatalf("real selected page children: %v", err)
	}
	if selected.HasChildren != (len(children) > 0) {
		t.Fatal("selected page's expand arrow does not match actual children")
	}
	text, warnings, err := markdown(p, base)
	if err != nil {
		t.Fatalf("live page conversion: %v", err)
	}
	if !strings.Contains(text, "# "+p.Title) {
		t.Fatal("converted page title is missing")
	}
	if dynamicMacros(p.Body.Storage.Value) && p.Body.ExportView.Value == "" {
		t.Fatal("dynamic macro rendering is missing")
	}
	blocks := regexp.MustCompile(`(?s)<ac:plain-text-body>\s*<!\[CDATA\[(.*?)\]\]>\s*</ac:plain-text-body>`).FindAllStringSubmatch(p.Body.Storage.Value, -1)
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for n, block := range blocks {
		if normalize(block[1]) != "" && !strings.Contains(normalize(text), normalize(block[1])) {
			t.Fatalf("source code macro %d content was not preserved", n+1)
		}
	}
	t.Logf("page=%s title=%q space=%s version=%d storage_chars=%d export_chars=%d markdown_chars=%d code_blocks_verified=%d dynamic=%t conversion_warnings=%d", p.ID, p.Title, p.Space.Key, p.Version.Number, len([]rune(p.Body.Storage.Value)), len([]rune(p.Body.ExportView.Value)), len([]rune(text)), len(blocks), dynamicMacros(p.Body.Storage.Value), len(warnings))
	for _, warning := range warnings {
		kind, _, _ := strings.Cut(warning, ":")
		t.Logf("conversion_warning_kind=%q", kind)
	}
	h := &collector{}
	cursor, err := connector.FetchStream(ctx, raw, nil, h)
	if err != nil {
		t.Fatalf("real connector streaming fetch: %v", err)
	}
	found := false
	attachments := 0
	for _, item := range h.items {
		if item.ExternalID == id {
			found = true
		}
		if item.Metadata["error"] != "" {
			t.Fatal("a source attachment could not be downloaded")
		}
		if item.Metadata["attachment"] == "true" {
			attachments++
			t.Logf("attachment_id=%s media_type=%s downloaded_bytes=%d image=%t", item.Metadata["source_attachment_id"], item.ContentType, len(item.Content), item.Metadata["confluence_image"] == "true")
		}
	}
	if !found {
		t.Fatal("selected page did not appear in connector output")
	}
	second := &collector{}
	_, err = connector.FetchStream(ctx, raw, cursor, second)
	if err != nil {
		t.Fatalf("real connector incremental fetch: %v", err)
	}
	t.Logf("first_fetch_items=%d attachments_downloaded=%d next_fetch_changed_items=%d (no upload, OCR, indexing or scheduler acceptance)", len(h.items), attachments, len(second.items))
}
