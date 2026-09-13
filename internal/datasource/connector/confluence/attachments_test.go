package confluence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestAttachmentsSyncIndependentlyOfParentVersion(t *testing.T) {
	allowTestServer(t)
	p := fixture("12", "Guide", 1)
	version := 1
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("download/list authentication missing")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/confluence/rest/api/content/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []page{p}})
		case "/confluence/rest/api/content/12":
			_ = json.NewEncoder(w).Encode(p)
		case "/confluence/rest/api/content/12/child/attachment":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{"id": "21", "title": "manual.pdf", "version": map[string]int{"number": version}, "metadata": map[string]string{"mediaType": "application/pdf"}, "_links": map[string]string{"download": "/download/attachments/12/manual.pdf"}},
				map[string]any{"id": "22", "title": "diagram.png", "version": map[string]int{"number": 1}, "metadata": map[string]string{"mediaType": "image/png"}, "_links": map[string]string{"download": "/confluence/download/attachments/12/diagram.png"}},
			}})
		case "/confluence/download/attachments/12/manual.pdf":
			downloads++
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-example"))
		case "/confluence/download/attachments/12/diagram.png":
			downloads++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNG-example"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL + "/confluence", "api_token": "test-token"}, ResourceIDs: []string{"space:OPS"}, MultimodalEnabled: true}
	c := NewConnector()
	h := &testHandler{committed: true}
	cursor, err := c.FetchStream(context.Background(), raw, nil, h)
	if err != nil || len(h.items) != 3 || downloads != 2 {
		t.Fatalf("initial attachment sync: items=%d downloads=%d err=%v", len(h.items), downloads, err)
	}
	if h.items[1].ExternalID != types.SubtreeChildID("12", "attachment", "21") || h.items[2].Metadata["confluence_image"] != "true" {
		t.Fatal("missing parent/image identity")
	}
	h = &testHandler{committed: true}
	cursor, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 0 || downloads != 2 {
		t.Fatal("unchanged attachments downloaded again")
	}
	version = 2
	h = &testHandler{committed: true}
	_, err = c.FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 1 || h.items[0].FileName != "manual.pdf" || downloads != 3 {
		t.Fatalf("attachment edit missed without parent edit: %v", err)
	}
}

func TestAttachmentRedirectDoesNotLeakCredentials(t *testing.T) {
	allowTestServer(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("site credentials leaked to CDN")
		}
		_, _ = w.Write([]byte("signed-file"))
	}))
	defer cdn.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("site authentication missing")
		}
		http.Redirect(w, r, cdn.URL+"/file?signature=test", http.StatusFound)
	}))
	defer site.Close()
	c := newClient(&config{BaseURL: site.URL, Token: "test-token"})
	data, err := c.download(context.Background(), "/download/attachments/12/doc.pdf")
	if err != nil || string(data) != "signed-file" {
		t.Fatalf("signed CDN redirect failed: %v", err)
	}
	if _, err := c.download(context.Background(), cdn.URL+"/file"); err == nil {
		t.Fatal("accepted off-site authenticated download link")
	}
}

type attachmentFailureHandler struct{ testHandler }

func (h *attachmentFailureHandler) Emit(ctx context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	if item.Metadata["error"] != "" {
		return datasource.ErrIngestFailed
	}
	return nil
}

func TestImagesWithoutVLMReportFailureAndRemainRetryable(t *testing.T) {
	allowTestServer(t)
	p := fixture("12", "Guide", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/child/attachment") {
			_, _ = w.Write([]byte(`{"results":[{"id":"22","title":"diagram.png","version":{"number":1},"metadata":{"mediaType":"image/png"},"_links":{"download":"/download/image"}}]}`))
		} else if strings.HasSuffix(r.URL.Path, "/search") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []page{p}})
		} else {
			_ = json.NewEncoder(w).Encode(p)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]any{"base_url": server.URL, "api_token": "test-token"}, ResourceIDs: []string{"page:12"}}
	h := &attachmentFailureHandler{testHandler: testHandler{committed: true}}
	cursor, err := NewConnector().FetchStream(context.Background(), raw, nil, h)
	if err != nil || len(h.items) != 2 || !strings.Contains(h.items[1].Metadata["error"], "visual model") {
		t.Fatalf("missing actionable VLM failure: %v", err)
	}
	h = &attachmentFailureHandler{testHandler: testHandler{committed: true}}
	_, err = NewConnector().FetchStream(context.Background(), raw, cursor, h)
	if err != nil || len(h.items) != 1 || h.items[0].Metadata["error"] == "" {
		t.Fatal("failed image revision was committed")
	}
}
