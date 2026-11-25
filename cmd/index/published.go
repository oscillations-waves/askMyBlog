package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

type post struct {
	slug  string
	title string
	date  string
	tags  string
	text  string
}

type feed struct {
	XMLName xml.Name `xml:"rss"`
	Items   []struct {
		Link       string   `xml:"link"`
		Title      string   `xml:"title"`
		Date       string   `xml:"pubDate"`
		Categories []string `xml:"category"`
	} `xml:"channel>item"`
}

func fetchPublishedPosts(ctx context.Context, client *http.Client, siteURL string) ([]post, error) {
	site, err := url.Parse(siteURL)
	if err != nil || (site.Scheme != "http" && site.Scheme != "https") || site.Host == "" || site.User != nil || site.RawQuery != "" || site.Fragment != "" {
		return nil, fmt.Errorf("BLOG_SITE_URL must be an HTTP or HTTPS site URL without credentials, a query, or a fragment")
	}
	site.Path = strings.TrimRight(site.Path, "/") + "/"
	site.RawPath = ""

	// The RSS feed is public, so restrict requests to the configured site and paths.
	fetcher := *client
	fetcher.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	feedURL := site.ResolveReference(&url.URL{Path: "rss.xml"})
	data, err := fetchPage(ctx, &fetcher, feedURL)
	if err != nil {
		return nil, fmt.Errorf("fetch RSS feed: %w", err)
	}
	var rss feed
	if err := xml.Unmarshal(data, &rss); err != nil {
		return nil, fmt.Errorf("parse RSS feed: %w", err)
	}

	var posts []post
	seen := make(map[string]bool)
	for _, item := range rss.Items {
		slug := publishedSlug(item.Link, site)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		articleURL := site.ResolveReference(&url.URL{Path: "blog/" + slug + "/"})
		page, err := fetchPage(ctx, &fetcher, articleURL)
		if err != nil {
			return nil, fmt.Errorf("fetch blog post %s: %w", slug, err)
		}
		plain, err := articleText(page)
		if err != nil {
			return nil, fmt.Errorf("read blog post %s: %w", slug, err)
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = slug
		}
		posts = append(posts, post{slug: slug, title: title, date: item.Date, tags: strings.Join(item.Categories, ", "), text: plain})
	}
	if len(posts) == 0 {
		return nil, fmt.Errorf("RSS contains no published blog posts at %s", feedURL)
	}
	return posts, nil
}

var slugPattern = regexp.MustCompile(`^[[:alnum:]_-]+$`)

func publishedSlug(link string, site *url.URL) string {
	item, err := url.Parse(strings.TrimSpace(link))
	if err != nil || item.User != nil || item.Opaque != "" || item.RawQuery != "" || item.ForceQuery || item.Fragment != "" {
		return ""
	}
	if (item.IsAbs() || item.Host != "") && (item.Scheme != site.Scheme || !strings.EqualFold(item.Host, site.Host)) {
		return ""
	}
	itemPath := item.Path
	if !strings.HasPrefix(itemPath, "/") {
		itemPath = site.Path + itemPath
	}
	slug, found := strings.CutPrefix(itemPath, site.Path+"blog/")
	if !found {
		// Older feeds used root-relative paths for GitHub Pages project sites.
		slug, found = strings.CutPrefix(itemPath, "/blog/")
	}
	slug = strings.TrimSuffix(slug, "/")
	if !found || !slugPattern.MatchString(slug) {
		return ""
	}
	return slug
}

func fetchPage(ctx context.Context, client *http.Client, page *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxPageSize = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPageSize {
		return nil, fmt.Errorf("response exceeds %d bytes", maxPageSize)
	}
	return data, nil
}

func articleText(page []byte) (string, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return "", err
	}
	article := findArticle(doc)
	if article == nil {
		return "", fmt.Errorf("page has no article")
	}
	var contents strings.Builder
	writeArticleText(&contents, article)
	plain := strings.Join(strings.Fields(contents.String()), " ")
	if plain == "" {
		return "", fmt.Errorf("article has no searchable text")
	}
	return plain, nil
}

func findArticle(node *html.Node) *html.Node {
	if node.Type == html.ElementNode && node.Data == "article" {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findArticle(child); found != nil {
			return found
		}
	}
	return nil
}

func writeArticleText(out *strings.Builder, node *html.Node) {
	if node.Type == html.TextNode {
		out.WriteString(node.Data)
		return
	}
	if node.Type == html.ElementNode {
		switch node.Data {
		case "script", "style", "template", "svg":
			return
		case "h1", "h2", "h3", "h4", "h5", "h6", "br", "p", "li", "div", "figcaption", "blockquote", "td", "th", "pre", "dt", "dd":
			out.WriteByte(' ')
			defer out.WriteByte(' ')
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		writeArticleText(out, child)
	}
}
