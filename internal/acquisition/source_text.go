package acquisition

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// SourceText is the complete visible text of the fetched response's main/article
// regions, or the body when no such regions exist. It does not follow external
// links or claim that unavailable attachments were acquired.
func SourceText(body string) (string, error) {
	if !utf8.ValidString(body) {
		return "", fmt.Errorf("source text is not decoded UTF-8")
	}
	if json.Valid([]byte(body)) || !strings.Contains(body, "<") {
		return strings.Join(strings.Fields(body), " "), nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return "", err
	}
	doc.Find("script,style,noscript,template,svg,canvas,iframe,nav,[role=navigation],[aria-hidden=true],[hidden]").Remove()
	roots := doc.Find("main,article,[role=main]")
	if roots.Length() == 0 {
		roots = doc.Find("body")
	}
	chosen := map[*html.Node]bool{}
	for _, n := range roots.Nodes {
		chosen[n] = true
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "br", "p", "div", "section", "article", "main", "li", "tr", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "blockquote":
				out.WriteByte(' ')
			}
		}
	}
	for _, n := range roots.Nodes {
		nested := false
		for parent := n.Parent; parent != nil; parent = parent.Parent {
			if chosen[parent] {
				nested = true
				break
			}
		}
		if !nested {
			visit(n)
			out.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(out.String()), " "), nil
}

// CleanText extracts cleaned, line- and paragraph-structured plain text from HTML
// or text responses for clean persistence and semantic RAG chunking.
func CleanText(body string) (string, error) {
	if !utf8.ValidString(body) {
		return "", fmt.Errorf("source text is not decoded UTF-8")
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", nil
	}
	if json.Valid([]byte(trimmed)) || !strings.Contains(trimmed, "<") {
		lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\r", "\n"), "\n")
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.Join(strings.Fields(line), " ")
			if line != "" {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n"), nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return "", err
	}
	doc.Find("script,style,noscript,template,svg,canvas,iframe,[aria-hidden=true],[hidden]").Remove()
	roots := doc.Find("main,article,[role=main]")
	if roots.Length() == 0 {
		roots = doc.Find("body")
	}
	if roots.Length() == 0 {
		roots = doc.Selection
	}
	chosen := map[*html.Node]bool{}
	for _, n := range roots.Nodes {
		chosen[n] = true
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.Join(strings.Fields(n.Data), " ")
			if text != "" {
				if out.Len() > 0 {
					last := out.String()[out.Len()-1]
					if last != '\n' && last != ' ' && last != '\t' {
						out.WriteByte(' ')
					}
				}
				out.WriteString(text)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "td", "th":
				out.WriteString(" | ")
			case "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				out.WriteByte('\n')
			case "p", "div", "section", "article", "main", "pre", "blockquote", "table":
				out.WriteString("\n\n")
			}
		}
	}
	for _, n := range roots.Nodes {
		nested := false
		for parent := n.Parent; parent != nil; parent = parent.Parent {
			if chosen[parent] {
				nested = true
				break
			}
		}
		if !nested {
			visit(n)
			out.WriteString("\n\n")
		}
	}
	rawLines := strings.Split(out.String(), "\n")
	cleaned := make([]string, 0, len(rawLines))
	blank := false
	for _, line := range rawLines {
		line = strings.TrimSpace(strings.Trim(line, "|"))
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank && len(cleaned) > 0 {
				cleaned = append(cleaned, "")
			}
			blank = true
			continue
		}
		blank = false
		cleaned = append(cleaned, line)
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n")), nil
}
