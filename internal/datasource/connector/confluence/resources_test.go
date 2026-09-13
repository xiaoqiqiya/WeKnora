package confluence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestConfluenceResourceChildrenAndSpaceHomepage(t *testing.T) {
	allowTestServer(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rest/api/space" {
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"key": "OPS", "name": "运维"}}})
			return
		}
		if !strings.Contains(r.URL.Query().Get("expand"), "children.page") {
			t.Error("child expansion is missing")
		}
		root := fixture("12", "运维", 1)
		root.Children.Page = &collection[page]{Size: 2}
		leaf := fixture("13", "Leaf", 1)
		leaf.Children.Page = &collection[page]{}
		branch := fixture("14", "Branch", 1)
		branch.Children.Page = &collection[page]{Results: []page{fixture("15", "Child", 1)}}
		if r.URL.Path == "/rest/api/content" {
			json.NewEncoder(w).Encode(collection[page]{Results: []page{root}})
		} else if r.URL.Path == "/rest/api/content/12/child/page" {
			json.NewEncoder(w).Encode(collection[page]{Results: []page{leaf, branch}})
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	raw := &types.DataSourceConfig{Credentials: map[string]interface{}{"base_url": server.URL, "api_token": "test-token"}}
	c := NewConnector()
	spaces, err := c.ListResources(context.Background(), raw, "")
	if err != nil || len(spaces) != 1 || spaces[0].ExternalID != "space:OPS" {
		t.Fatalf("spaces: %v %v", spaces, err)
	}
	roots, err := c.ListResources(context.Background(), raw, "space:OPS")
	if err != nil || len(roots) != 1 || roots[0].ParentID != "space:OPS" || !roots[0].HasChildren {
		t.Fatalf("homepage: %v %v", roots, err)
	}
	children, err := c.ListResources(context.Background(), raw, "page:12")
	if err != nil || len(children) != 2 || children[0].HasChildren || !children[1].HasChildren {
		t.Fatalf("page children: %v %v", children, err)
	}
}
