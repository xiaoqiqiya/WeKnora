package confluence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

type attachment struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version struct {
		Number int       `json:"number"`
		When   time.Time `json:"when"`
	} `json:"version"`
	Metadata struct {
		MediaType string `json:"mediaType"`
	} `json:"metadata"`
	Extensions struct {
		FileSize int64 `json:"fileSize"`
	} `json:"extensions"`
	Links struct {
		Download string `json:"download"`
		WebUI    string `json:"webui"`
	} `json:"_links"`
}

func settingEnabled(raw *types.DataSourceConfig, key string) bool {
	value, ok := raw.Settings[key].(bool)
	return !ok || value
}

func (c *client) download(ctx context.Context, rawLink string) ([]byte, error) {
	site, _ := url.Parse(c.config.BaseURL + "/")
	link, err := url.Parse(rawLink)
	if err != nil || rawLink == "" || link.User != nil {
		return nil, fmt.Errorf("invalid confluence attachment download link")
	}
	contextPath := strings.TrimRight(site.Path, "/")
	if !link.IsAbs() && link.Host == "" && strings.HasPrefix(link.Path, "/") && contextPath != "" && !strings.HasPrefix(link.Path, contextPath+"/") {
		link.Path = contextPath + link.Path
	}
	endpoint := site.ResolveReference(link)
	if endpoint.Scheme != site.Scheme || endpoint.Host != site.Host || (contextPath != "" && !strings.HasPrefix(endpoint.Path, contextPath+"/")) {
		return nil, fmt.Errorf("confluence attachment link is outside the configured site")
	}
	transport := datasource.NewConnectorHTTPClient(60 * time.Second)
	safeRedirect := transport.CheckRedirect
	transport.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("attachment redirect limit reached")
		}
		if safeRedirect != nil {
			if err := safeRedirect(req, via); err != nil {
				return err
			}
		}
		// Cloud downloads may redirect to signed CDN URLs. Do not forward the PAT,
		// Basic credentials or cookies, even to another subdomain of the site.
		if req.URL.Scheme != site.Scheme || req.URL.Host != site.Host || (contextPath != "" && !strings.HasPrefix(req.URL.Path, contextPath+"/")) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}
	return c.fetch(ctx, endpoint.String(), transport, 100*1024*1024, false)
}

func emitRevision(ctx context.Context, h datasource.StreamHandler, item *types.FetchedItem, state *cursorState) error {
	if err := h.Emit(ctx, *item); err != nil {
		return err
	}
	committed := true
	if ack, ok := h.(datasource.CommitAwareStreamHandler); ok {
		var err error
		committed, err = ack.ItemCommitted(ctx, item)
		if err != nil {
			return err
		}
	}
	if committed {
		state.Versions[item.ExternalID] = item.Metadata["source_revision"]
	}
	return h.Checkpoint(ctx, syncCursor(*state))
}

func syncAttachments(ctx context.Context, c *client, raw *types.DataSourceConfig, parent page, resourceID string, state *cursorState, seen map[string]bool, h datasource.StreamHandler) error {
	if !settingEnabled(raw, "attachments") {
		return nil
	}
	return visit(ctx, c, "/content/"+parent.ID+"/child/attachment", url.Values{"expand": {"version,metadata,extensions"}}, func(a attachment) error {
		if a.ID == "" || a.Title == "" || a.Version.Number <= 0 {
			return fmt.Errorf("confluence attachment is missing ID, filename or version")
		}
		id := types.SubtreeChildID(parent.ID, "attachment", a.ID)
		seen[id] = true
		isImage := strings.HasPrefix(a.Metadata.MediaType, "image/")
		if isImage && !settingEnabled(raw, "images") {
			return nil
		}
		stamp := hash([]any{a.ID, a.Version.Number, a.Title, parent.Title, parent.Space.Key})
		if state.Versions[id] == stamp {
			return nil
		}
		meta := map[string]string{"channel": "confluence", "source_site": c.config.BaseURL, "source_page_id": parent.ID, "source_attachment_id": a.ID, "source_space_key": parent.Space.Key, "source_version": fmt.Sprint(a.Version.Number), "source_revision": stamp, "source_url": c.webURL(parent.Links.WebUI), "attachment": "true", "parent_page_title": parent.Title}
		if isImage {
			meta["confluence_image"] = "true"
		}
		name := safeFileName(strings.TrimSuffix(path.Base(a.Title), path.Ext(a.Title))) + strings.ToLower(path.Ext(a.Title))
		item := types.FetchedItem{ExternalID: id, Title: a.Title, FileName: name, ContentType: a.Metadata.MediaType, URL: c.webURL(parent.Links.WebUI), SourceResourceID: resourceID, UpdatedAt: a.Version.When, Metadata: meta}
		if isImage && !raw.MultimodalEnabled {
			item.URL = ""
			item.Metadata["error"] = "Confluence image OCR requires a visual model and multimodal parsing in the target knowledge base"
			err := h.Emit(ctx, item)
			if errors.Is(err, datasource.ErrIngestFailed) {
				return nil
			}
			return err
		}
		var data []byte
		var err error
		if a.Extensions.FileSize > 100*1024*1024 {
			err = fmt.Errorf("attachment exceeds 100 MiB")
		} else {
			data, err = c.download(ctx, a.Links.Download)
		}
		if err == nil && len(data) == 0 {
			err = fmt.Errorf("attachment is empty")
		}
		if err != nil {
			item.URL = ""
			item.Metadata["error"] = fmt.Sprintf("Confluence attachment %s download failed: %v", a.Title, err)
			err = h.Emit(ctx, item)
			if errors.Is(err, datasource.ErrIngestFailed) {
				return nil
			}
			return err
		}
		item.Content = data
		err = emitRevision(ctx, h, &item, state)
		if errors.Is(err, datasource.ErrIngestFailed) {
			return nil
		}
		return err
	})
}
