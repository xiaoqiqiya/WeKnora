package confluence

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/PuerkitoBio/goquery"
)

var cdata = regexp.MustCompile(`(?s)<!\[CDATA\[(.*?)\]\]>`)
var emptyNamespaceTag = regexp.MustCompile(`<((?:ac|ri):[\w-]+)([^<>]*?)/>`)
var macroName = regexp.MustCompile(`<ac:structured-macro\b[^>]*\bac:name=["']([^"']+)["']`)

func dynamicMacros(storage string) bool {
	for _, match := range macroName.FindAllStringSubmatch(storage, -1) {
		switch match[1] {
		case "code", "noformat", "info", "note", "warning", "tip", "panel", "expand", "toc", "anchor", "excerpt":
		default:
			return true
		}
	}
	return strings.Contains(storage, "<ac:adf-extension")
}

func descendants(s *goquery.Selection, name string) *goquery.Selection {
	return s.Find("*").FilterFunction(func(_ int, node *goquery.Selection) bool { return node.Nodes[0].Data == name })
}

func markdown(p page, baseURL string) (string, []string, error) {
	raw := p.Body.Storage.Value
	if p.Body.ExportView.Value != "" {
		raw = p.Body.ExportView.Value
	}
	raw = cdata.ReplaceAllStringFunc(raw, func(value string) string { return html.EscapeString(value[9 : len(value)-3]) })
	raw = emptyNamespaceTag.ReplaceAllString(raw, "<$1$2></$1>")
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	warnings := []string{}
	if p.Body.ExportView.Value != "" && doc.Find(".aui-message-error, .macro-error, .confluence-information-macro-failure, .error").Length() > 0 {
		return "", nil, fmt.Errorf("Confluence macro rendering failed; check source page permissions and plugins")
	}
	macros := descendants(doc.Selection, "ac:structured-macro")
	for i := macros.Length() - 1; i >= 0; i-- {
		macro := macros.Eq(i)
		name, _ := macro.Attr("ac:name")
		body := descendants(macro, "ac:rich-text-body").First()
		switch name {
		case "code", "noformat":
			text := descendants(macro, "ac:plain-text-body").First().Text()
			language := ""
			descendants(macro, "ac:parameter").Each(func(_ int, parameter *goquery.Selection) {
				if name, _ := parameter.Attr("ac:name"); name == "language" {
					language = parameter.Text()
				}
			})
			macro.ReplaceWithHtml("<pre><code class=\"language-" + html.EscapeString(language) + "\">" + html.EscapeString(text) + "</code></pre>")
		case "info", "note", "warning", "tip", "panel", "expand":
			inner, _ := body.Html()
			macro.ReplaceWithHtml("<blockquote>" + inner + "</blockquote>")
		case "toc", "anchor":
			macro.Remove()
		default:
			warnings = append(warnings, "unsupported macro: "+name)
			inner, _ := body.Html()
			macro.ReplaceWithHtml("<p>[Confluence macro: " + html.EscapeString(name) + "]</p>" + inner)
		}
	}
	descendants(doc.Selection, "ac:link").Each(func(_ int, link *goquery.Selection) {
		target := descendants(link, "ri:page").First()
		id, _ := target.Attr("ri:content-id")
		title, _ := target.Attr("ri:content-title")
		spaceKey, _ := target.Attr("ri:space-key")
		if spaceKey == "" {
			spaceKey = p.Space.Key
		}
		text := strings.TrimSpace(descendants(link, "ac:plain-text-link-body").Text())
		if text == "" {
			text = strings.TrimSpace(descendants(link, "ac:link-body").Text())
		}
		if text == "" {
			text = title
		}
		if id != "" {
			link.ReplaceWithHtml("<a href=\"" + html.EscapeString(baseURL+"/pages/viewpage.action?pageId="+url.QueryEscape(id)) + "\">" + html.EscapeString(text) + "</a>")
		} else if title != "" {
			link.ReplaceWithHtml("<a href=\"" + html.EscapeString(baseURL+"/display/"+url.PathEscape(spaceKey)+"/"+url.QueryEscape(title)) + "\">" + html.EscapeString(text) + "</a>")
		} else {
			warnings = append(warnings, "attachment or unresolved page link")
			link.ReplaceWithHtml("<span>" + html.EscapeString(text+" [Confluence attachment/link]") + "</span>")
		}
	})
	descendants(doc.Selection, "ac:image").Each(func(_ int, image *goquery.Selection) {
		attachment := descendants(image, "ri:attachment").First()
		name, _ := attachment.Attr("ri:filename")
		warnings = append(warnings, "image reference; attachment imported separately: "+name)
		image.ReplaceWithHtml("<p>[Confluence image: " + html.EscapeString(name) + "]</p>")
	})
	doc.Find("script, style, iframe").Remove()
	doc.Find("img").Each(func(_ int, image *goquery.Selection) {
		name, _ := image.Attr("alt")
		if name == "" {
			name, _ = image.Attr("src")
		}
		warnings = append(warnings, "image reference; attachment imported separately: "+name)
		image.ReplaceWithHtml("<p>[Confluence image: " + html.EscapeString(name) + "]</p>")
	})
	origin, _ := url.Parse(baseURL + "/")
	doc.Find("a[href], img[src]").Each(func(_ int, node *goquery.Selection) {
		attr := "href"
		if node.Nodes[0].Data == "img" {
			attr = "src"
		}
		value, _ := node.Attr(attr)
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") {
			node.RemoveAttr(attr)
			return
		}
		node.SetAttr(attr, origin.ResolveReference(u).String())
	})
	body, err := doc.Find("body").Html()
	if err != nil {
		return "", warnings, err
	}
	conv := converter.NewConverter(converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
	text, err := conv.ConvertString(body)
	if err != nil {
		return "", warnings, err
	}
	path := []string{p.Space.Key}
	for _, ancestor := range p.Ancestors {
		path = append(path, ancestor.Title)
	}
	path = append(path, p.Title)
	source := baseURL + "/pages/viewpage.action?pageId=" + url.QueryEscape(p.ID)
	return fmt.Sprintf("# %s\n\nSource: %s\n\nVersion: %d\n\nLocation: %s\n\n%s\n", strings.ReplaceAll(p.Title, "\n", " "), source, p.Version.Number, strings.Join(path, " / "), strings.TrimSpace(text)), warnings, nil
}
