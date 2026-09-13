// Package confluence syncs published pages through the Confluence v1 REST API.
package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

type config struct {
	BaseURL  string `json:"base_url"`
	Token    string `json:"api_token"`
	Username string `json:"username"`
}

func parseConfig(raw *types.DataSourceConfig) (*config, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: config is required", datasource.ErrInvalidConfig)
	}
	b, err := json.Marshal(raw.Credentials)
	if err != nil {
		return nil, fmt.Errorf("%w: credentials", datasource.ErrInvalidConfig)
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%w: credentials must be strings", datasource.ErrInvalidConfig)
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: base_url must be an HTTP(S) site URL without credentials, query or fragment", datasource.ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("%w: api_token is required", datasource.ErrInvalidCredentials)
	}
	if strings.ContainsAny(cfg.Token, "\r\n") {
		return nil, fmt.Errorf("%w: invalid api_token", datasource.ErrInvalidCredentials)
	}
	if err := datasource.ValidateConnectorBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type page struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Type   string `json:"type"`
	Space  struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	Ancestors []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"ancestors"`
	Children struct {
		Page *collection[page] `json:"page"`
	} `json:"children"`
	Version struct {
		Number int       `json:"number"`
		When   time.Time `json:"when"`
	} `json:"version"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
		ExportView struct {
			Value string `json:"value"`
		} `json:"export_view"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type space struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type collection[T any] struct {
	Results []T `json:"results"`
	Start   int `json:"start"`
	Limit   int `json:"limit"`
	Size    int `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

type client struct {
	config *config
	http   *http.Client
}

func newClient(cfg *config) *client {
	c := datasource.NewConnectorHTTPClient(30 * time.Second)
	// Authenticated API calls must never redirect to another origin or a login page.
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &client{config: cfg, http: c}
}

func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := c.config.BaseURL + "/rest/api" + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	body, err := c.fetch(ctx, endpoint, c.http, 32*1024*1024, true)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode confluence JSON: %w", err)
	}
	return nil
}

func (c *client) fetch(ctx context.Context, endpoint string, transport *http.Client, limit int64, requireJSON bool) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if !requireJSON {
			req.Header.Set("Accept", "*/*")
		}
		if c.config.Username != "" {
			req.SetBasicAuth(c.config.Username, c.config.Token)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.config.Token)
		}
		resp, err := transport.Do(req)
		if err != nil {
			return nil, fmt.Errorf("confluence request failed: %w", err)
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < 2 {
			resp.Body.Close()
			delay := time.Duration(1<<attempt) * time.Second
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 {
				delay = time.Duration(seconds) * time.Second
			} else if until, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
				delay = max(0, time.Until(until))
			}
			timer := time.NewTimer(max(100*time.Millisecond, delay))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return nil, fmt.Errorf("%w: confluence HTTP %d", datasource.ErrInvalidCredentials, resp.StatusCode)
			}
			return nil, fmt.Errorf("confluence HTTP %d", resp.StatusCode)
		}
		if requireJSON && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
			resp.Body.Close()
			return nil, fmt.Errorf("confluence returned non-JSON; check site URL and authentication")
		}
		// Bound individual responses; never include document bodies or tokens in errors.
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read confluence response: %w", err)
		}
		if int64(len(body)) > limit {
			return nil, fmt.Errorf("confluence response exceeds %d MiB", limit/(1024*1024))
		}
		return body, nil
	}
	return nil, fmt.Errorf("confluence retry limit reached")
}

// Each next link contributes only its numeric offset; never follow its host/path with credentials.
func visit[T any](ctx context.Context, c *client, path string, query url.Values, emit func(T) error) error {
	query = maps.Clone(query)
	start := 0
	for {
		query.Set("start", strconv.Itoa(start))
		query.Set("limit", "100")
		var result collection[T]
		if err := c.get(ctx, path, query, &result); err != nil {
			return err
		}
		for _, item := range result.Results {
			if err := emit(item); err != nil {
				return err
			}
		}
		if result.Links.Next == "" {
			return nil
		}
		next, err := url.Parse(result.Links.Next)
		if err != nil {
			return fmt.Errorf("invalid confluence pagination link")
		}
		offset, err := strconv.Atoi(next.Query().Get("start"))
		if err != nil || offset <= start || len(result.Results) == 0 {
			return fmt.Errorf("confluence pagination did not advance")
		}
		start = offset
	}
}

func (c *client) page(ctx context.Context, id string) (page, error) {
	var p page
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return p, fmt.Errorf("invalid confluence page ID")
	}
	err := c.get(ctx, "/content/"+url.PathEscape(id), url.Values{"expand": {"body.storage,body.export_view,version,space,ancestors"}}, &p)
	if err == nil && (p.ID != id || p.Type != "page" || p.Status != "current" || p.Version.Number <= 0) {
		err = fmt.Errorf("confluence page %s is not a valid current page", id)
	}
	return p, err
}

func (c *client) webURL(path string) string {
	if path == "" {
		return c.config.BaseURL
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" {
		return c.config.BaseURL
	}
	return c.config.BaseURL + "/" + strings.TrimLeft(path, "/")
}
