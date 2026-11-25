package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFetchPublishedPostsUsesOnlyBlogArticlesFromRSS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shravani.roy/rss.xml":
			origin := "http://" + r.Host
			fmt.Fprintf(w, `<rss version="2.0"><channel>
<item><title>Canonical title</title><link>%[1]s/shravani.roy/blog/canonical/</link><pubDate>Tue, 24 Nov 2026 00:00:00 GMT</pubDate><category>go</category></item>
<item><title>Legacy title</title><link>%[1]s/blog/legacy/</link></item>
<item><title>Duplicate</title><link>%[1]s/shravani.roy/blog/legacy/</link></item>
<item><title>Project</title><link>%[1]s/shravani.roy/projects/portfolio/</link></item>
<item><title>External</title><link>https://example.net/blog/untrusted/</link></item>
<item><title>Malformed</title><link>%[1]s/shravani.roy/blog/../hidden/</link></item>
<item><title>Nested</title><link>%[1]s/shravani.roy/blog/group/nested/</link></item>
<item><title>Encoded slash</title><link>%[1]s/shravani.roy/blog/group%%2Fnested/</link></item>
<item><title>Query</title><link>%[1]s/shravani.roy/blog/malformed/?draft=true</link></item>
</channel></rss>`, origin)
		case "/shravani.roy/blog/canonical/":
			fmt.Fprint(w, `<html><header>Ignored site header</header><main><article>
<h2>Visible heading</h2><p>Rendered <em>text</em> &amp; examples.</p><li>One item</li>
<script>Ignore scripts</script><style>Ignore styles</style>
</article></main><footer>Ignored site footer</footer></html>`)
		case "/shravani.roy/blog/legacy/":
			fmt.Fprint(w, `<main><article><p>Legacy URL, canonical content.</p></article></main>`)
		default:
			t.Errorf("unexpected request to %q", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := fetchPublishedPosts(context.Background(), server.Client(), server.URL+"/shravani.roy")
	if err != nil {
		t.Fatal(err)
	}
	want := []post{
		{slug: "canonical", title: "Canonical title", date: "Tue, 24 Nov 2026 00:00:00 GMT", tags: "go", text: "Visible heading Rendered text & examples. One item"},
		{slug: "legacy", title: "Legacy title", text: "Legacy URL, canonical content."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published posts = %+v, want %+v", got, want)
	}
}

func TestFetchPublishedPostsFailsWhenAnArticleIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rss.xml":
			fmt.Fprint(w, `<rss><channel><item><link>/blog/available/</link></item><item><link>/blog/missing/</link></item></channel></rss>`)
		case "/blog/available/":
			fmt.Fprint(w, `<article>Available post text.</article>`)
		default:
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	got, err := fetchPublishedPosts(context.Background(), server.Client(), server.URL)
	if len(got) != 0 || err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("published posts = %+v, error = %v, want empty result and HTTP 503 error", got, err)
	}
}

func TestLoadPostsPrefersExplicitLocalBlogPath(t *testing.T) {
	blogPath := t.TempDir()
	postDir := filepath.Join(blogPath, "local-post")
	if err := os.Mkdir(postDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(postDir, "index.md"), []byte("---\ntitle: Local title\ntags: [go]\n---\n\n# Local text.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLOG_PATH", blogPath)
	t.Setenv("BLOG_SITE_URL", "not a site URL")
	posts, err := loadPosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []post{{slug: "local-post", title: "Local title", tags: "go", text: "Local text."}}
	if !reflect.DeepEqual(posts, want) {
		t.Fatalf("local posts = %+v, want %+v", posts, want)
	}
}
