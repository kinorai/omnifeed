// Command gomd is the Go converter candidate for benchmark/fetch/run.py: one
// plain GET, then HTML to markdown in Go, the way a plain-HTTP first step in
// omnifeed's generic engine would do it. It is a separate module so the
// converter libraries stay out of omnifeed's go.mod until one is chosen.
//
//	go run . -addr 127.0.0.1:18090
//	GET /?mode=raw&url=...   the page minus omnifeed's excluded tags
//	GET /?mode=read&url=...  Readability's main content first
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

// excluded mirrors the excluded_tags omnifeed sends crawl4ai
// (internal/engine/crawl4ai/engine.go), so both see the same page.
var excluded = []string{"nav", "footer", "header", "form", "aside", "script", "style", "noscript", "template"}

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

type reply struct {
	Markdown string `json:"markdown"`
	Error    string `json:"error,omitempty"`
}

func newConverter() *converter.Converter {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
	for _, tag := range excluded {
		conv.Register.TagType(tag, converter.TagTypeRemove, converter.PriorityStandard)
	}
	return conv
}

func convert(client *http.Client, conv *converter.Converter, rawURL, mode string) (string, error) {
	page, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "html") {
		return "", fmt.Errorf("content-type %s", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	src := body
	if mode == "read" {
		article, err := readability.FromReader(bytes.NewReader(body), resp.Request.URL)
		if err != nil {
			return "", fmt.Errorf("readability: %w", err)
		}
		if article.Node == nil {
			return "", fmt.Errorf("readability: no content")
		}
		var buf bytes.Buffer
		if err := article.RenderHTML(&buf); err != nil {
			return "", err
		}
		src = buf.Bytes()
	}
	md, err := conv.ConvertReader(bytes.NewReader(src), converter.WithDomain(page.Scheme+"://"+page.Host))
	return string(md), err
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	flag.Parse()
	client := &http.Client{Timeout: 30 * time.Second}
	conv := newConverter()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		md, err := convert(client, conv, r.URL.Query().Get("url"), r.URL.Query().Get("mode"))
		out := reply{Markdown: md}
		if err != nil {
			out.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	log.Fatal(http.ListenAndServe(*addr, nil))
}
