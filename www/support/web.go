package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var markup = regexp.MustCompile(`<[^>]+>`)

func clean(s string) string { return html.UnescapeString(markup.ReplaceAllString(s, "")) }
func readJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(target)
}

// Use fixed search providers. Visitors cannot choose a destination for server-side HTTP requests.
func webSources(ctx context.Context, q string) []source {
	ch := make(chan []source, 2)
	go func() {
		var data struct {
			Results []struct{ Title, URL, Description string }
		}
		var out []source
		if readJSON(ctx, "https://learn.microsoft.com/api/search?locale=zh-cn&%24top=3&search="+url.QueryEscape(q), &data) == nil {
			for _, v := range data.Results {
				if strings.HasPrefix(v.URL, "https://learn.microsoft.com/") {
					out = append(out, source{v.Title, v.URL, clean(v.Description)})
				}
			}
		}
		ch <- out
	}()
	go func() {
		var data struct {
			Query struct {
				Search []struct {
					Title, Snippet string
					PageID         int
				}
			}
		}
		var out []source
		if readJSON(ctx, "https://zh.wikipedia.org/w/api.php?action=query&list=search&format=json&srlimit=3&srsearch="+url.QueryEscape(q), &data) == nil {
			for _, v := range data.Query.Search {
				out = append(out, source{v.Title, "https://zh.wikipedia.org/wiki/" + url.PathEscape(v.Title), clean(v.Snippet)})
			}
		}
		ch <- out
	}()
	var out []source
	for range 2 {
		select {
		case s := <-ch:
			out = append(out, s...)
		case <-ctx.Done():
			return out
		}
	}
	return out
}
