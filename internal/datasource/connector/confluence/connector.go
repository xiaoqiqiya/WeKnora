package confluence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

type Connector struct{}

var _ datasource.StreamingConnector = (*Connector)(nil)

func NewConnector() *Connector  { return &Connector{} }
func (*Connector) Type() string { return types.ConnectorTypeConfluence }

func (*Connector) Validate(ctx context.Context, raw *types.DataSourceConfig) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	var result collection[space]
	return newClient(cfg).get(ctx, "/space", url.Values{"limit": {"1"}}, &result)
}

func parseResource(id string) (string, string, error) {
	kind, value, ok := strings.Cut(id, ":")
	if !ok || value == "" {
		return "", "", fmt.Errorf("invalid confluence resource %q", id)
	}
	if kind == "page" {
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return "", "", fmt.Errorf("invalid confluence page ID")
		}
	} else if kind != "space" {
		return "", "", fmt.Errorf("invalid confluence resource type")
	}
	return kind, value, nil
}

func (*Connector) ListResources(ctx context.Context, raw *types.DataSourceConfig, parentID string) ([]types.Resource, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	c := newClient(cfg)
	resources := []types.Resource{}
	if parentID == "" {
		err = visit(ctx, c, "/space", url.Values{}, func(s space) error {
			if s.Key == "" {
				return fmt.Errorf("confluence space key is missing")
			}
			resources = append(resources, types.Resource{ExternalID: "space:" + s.Key, Name: s.Name, Type: "space", URL: c.webURL(s.Links.WebUI), HasChildren: true})
			return nil
		})
		return resources, err
	}
	kind, value, err := parseResource(parentID)
	if err != nil {
		return nil, err
	}
	path := "/content"
	query := url.Values{"type": {"page"}, "status": {"current"}, "expand": {"version,space,ancestors,children.page"}}
	if kind == "space" {
		query.Set("spaceKey", value)
	} else {
		path = "/content/" + value + "/child/page"
	}
	err = visit(ctx, c, path, query, func(p page) error {
		if p.ID == "" {
			return fmt.Errorf("confluence page ID is missing")
		}
		if kind == "space" && len(p.Ancestors) != 0 {
			return nil
		}
		children := p.Children.Page
		// Older servers may omit the expansion; the UI resolves unknown nodes on first expand.
		hasChildren := children == nil || children.Size > 0 || len(children.Results) > 0 || children.Links.Next != ""
		resources = append(resources, types.Resource{ExternalID: "page:" + p.ID, Name: p.Title, Type: "page", ParentID: parentID, HasChildren: hasChildren, URL: c.webURL(p.Links.WebUI), ModifiedAt: p.Version.When})
		return nil
	})
	return resources, err
}

func (*Connector) ResolveResourceAncestors(ctx context.Context, raw *types.DataSourceConfig, ids []string) ([]string, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	c := newClient(cfg)
	set := map[string]bool{}
	for _, id := range ids {
		kind, value, err := parseResource(id)
		if err != nil {
			return nil, err
		}
		if kind == "space" {
			continue
		}
		p, err := c.page(ctx, value)
		if err != nil {
			return nil, err
		}
		set["space:"+p.Space.Key] = true
		for _, a := range p.Ancestors {
			set["page:"+a.ID] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

type cursorState struct {
	Scope    string            `json:"scope"`
	Versions map[string]string `json:"versions"`
	Dynamic  map[string]bool   `json:"dynamic"`
}

func hash(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func revision(p page) string {
	// Moves and title changes must refresh metadata even if a server keeps the content version.
	return hash([]any{p.Version.Number, p.Title, p.Space.Key, p.Ancestors})
}

func syncCursor(state cursorState) *types.SyncCursor {
	versions := make(map[string]interface{}, len(state.Versions))
	for id, value := range state.Versions {
		versions[id] = value
	}
	dynamic := make(map[string]interface{}, len(state.Dynamic))
	for id, value := range state.Dynamic {
		dynamic[id] = value
	}
	return &types.SyncCursor{LastSyncTime: time.Now().UTC(), ConnectorCursor: map[string]interface{}{"scope": state.Scope, "versions": versions, "dynamic": dynamic}}
}

func (*Connector) FetchStream(ctx context.Context, raw *types.DataSourceConfig, previous *types.SyncCursor, h datasource.StreamHandler) (*types.SyncCursor, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	if len(raw.ResourceIDs) == 0 {
		return nil, fmt.Errorf("select at least one confluence space or page")
	}
	label, _ := raw.Settings["label"].(string)
	label = strings.TrimSpace(label)
	ids := slices.Clone(raw.ResourceIDs)
	slices.Sort(ids)
	state := cursorState{Scope: hash([]any{cfg.BaseURL, ids, label, settingEnabled(raw, "attachments"), settingEnabled(raw, "images"), raw.MultimodalEnabled}), Versions: map[string]string{}, Dynamic: map[string]bool{}}
	if previous != nil && previous.ConnectorCursor != nil {
		b, err := json.Marshal(previous.ConnectorCursor)
		if err != nil {
			return nil, fmt.Errorf("invalid confluence cursor: %w", err)
		}
		var prior cursorState
		if err := json.Unmarshal(b, &prior); err != nil {
			return nil, fmt.Errorf("invalid confluence cursor: %w", err)
		}
		if prior.Scope == state.Scope && prior.Versions != nil {
			state.Versions = prior.Versions
			if prior.Dynamic != nil {
				state.Dynamic = prior.Dynamic
			}
		}
	}
	c := newClient(cfg)
	seen := map[string]bool{}
	// ponytail: scan page summaries each cycle; use CQL time windows only if metadata scans become costly.
	for _, resourceID := range ids {
		kind, value, err := parseResource(resourceID)
		if err != nil {
			return nil, err
		}
		filter := "space=" + strconv.Quote(value)
		if kind == "page" {
			filter = "(id=" + value + " OR ancestor=" + value + ")"
		}
		cql := "type=page AND " + filter
		if label != "" {
			cql += " AND label=" + strconv.Quote(label)
		}
		query := url.Values{"cql": {cql + " ORDER BY created ASC"}, "status": {"current"}, "expand": {"version,space,ancestors"}}
		err = visit(ctx, c, "/content/search", query, func(summary page) error {
			if summary.ID == "" || summary.Version.Number <= 0 {
				return fmt.Errorf("confluence page summary is missing ID or version")
			}
			if summary.Status != "current" || summary.Type != "page" || seen[summary.ID] {
				return nil
			}
			seen[summary.ID] = true
			if state.Versions[summary.ID] == revision(summary) && !state.Dynamic[summary.ID] {
				return syncAttachments(ctx, c, raw, summary, resourceID, &state, seen, h)
			}
			p, err := c.page(ctx, summary.ID)
			if err != nil {
				return err
			}
			state.Dynamic[p.ID] = dynamicMacros(p.Body.Storage.Value)
			if state.Dynamic[p.ID] && p.Body.ExportView.Value == "" {
				return fmt.Errorf("Confluence page %s contains dynamic macros but no export_view rendering; check source API permissions", p.ID)
			}
			content, warnings, err := markdown(p, cfg.BaseURL)
			if err != nil {
				return fmt.Errorf("convert confluence page %s: %w", p.ID, err)
			}
			stamp := revision(p)
			if state.Dynamic[p.ID] {
				stamp = hash([]any{stamp, content})
			}
			if state.Versions[p.ID] == stamp {
				return syncAttachments(ctx, c, raw, p, resourceID, &state, seen, h)
			}
			item := types.FetchedItem{
				ExternalID: p.ID, Title: p.Title, Content: []byte(content), ContentType: "text/markdown",
				FileName: "confluence-" + p.ID + "-" + safeFileName(p.Title) + ".md", URL: c.webURL(p.Links.WebUI),
				UpdatedAt: p.Version.When, SourceResourceID: resourceID,
				Metadata: map[string]string{"channel": "confluence", "source_site": cfg.BaseURL, "source_url": c.webURL(p.Links.WebUI), "source_page_id": p.ID, "source_space_key": p.Space.Key, "source_version": strconv.Itoa(p.Version.Number), "source_revision": stamp, "conversion_warnings": strings.Join(warnings, "; ")},
			}
			if err := emitRevision(ctx, h, &item, &state); err != nil {
				return err
			}
			return syncAttachments(ctx, c, raw, p, resourceID, &state, seen, h)
		})
		if err != nil {
			return nil, err
		}
	}
	for id := range state.Versions {
		if !seen[id] {
			delete(state.Versions, id)
			delete(state.Dynamic, id)
		}
	}
	next := syncCursor(state)
	return next, h.Checkpoint(ctx, next)
}

type collector struct{ items []types.FetchedItem }

func (h *collector) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return nil
}
func (*collector) Checkpoint(context.Context, *types.SyncCursor) error { return nil }

func (c *Connector) FetchAll(ctx context.Context, raw *types.DataSourceConfig, ids []string) ([]types.FetchedItem, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: config is required", datasource.ErrInvalidConfig)
	}
	copy := *raw
	copy.ResourceIDs = ids
	h := &collector{}
	_, err := c.FetchStream(ctx, &copy, nil, h)
	return h.items, err
}

func (c *Connector) FetchIncremental(ctx context.Context, raw *types.DataSourceConfig, cursor *types.SyncCursor) ([]types.FetchedItem, *types.SyncCursor, error) {
	h := &collector{}
	next, err := c.FetchStream(ctx, raw, cursor, h)
	return h.items, next, err
}

func safeFileName(title string) string {
	title = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune("<>:\"/\\|?*", r) {
			return '-'
		}
		return r
	}, title)
	runes := []rune(strings.Trim(title, " ."))
	if len(runes) > 100 {
		runes = runes[:100]
	}
	if len(runes) == 0 {
		return "page"
	}
	return string(runes)
}
