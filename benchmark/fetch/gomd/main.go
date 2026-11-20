// Command gomd is the Go converter candidate for benchmark/fetch/run.py: one
// plain GET, then HTML to markdown in Go, the way a plain-HTTP first step in
// omnifeed's generic engine would do it. It is a separate module so the
// converter libraries stay out of omnifeed's go.mod until one is chosen.
//
//	go build -o /tmp/gomd .
//	/tmp/gomd -mode raw  URL   the page minus omnifeed's excluded tags
//	/tmp/gomd -mode read URL   Readability's main content first
//
// It prints one JSON object: {"markdown": ..., "error": ...}.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

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
	// The converters parse UTF-8 only: decode a Latin-1 or Shift_JIS page
	// from its Content-Type or <meta charset> first.
	utf8Body, err := charset.NewReader(io.LimitReader(resp.Body, 10<<20), resp.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(utf8Body)
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
	// Links resolve against the page after redirects, as Readability's do.
	page := resp.Request.URL
	md, err := conv.ConvertReader(bytes.NewReader(src), converter.WithDomain(page.Scheme+"://"+page.Host))
	return string(md), err
}

func main() {
	mode := flag.String("mode", "raw", "raw, or read for Readability first")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: gomd [-mode raw|read] URL")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	md, err := convert(client, newConverter(), flag.Arg(0), *mode)
	out := reply{Markdown: md}
	if err != nil {
		out.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatal(err)
	}
}
