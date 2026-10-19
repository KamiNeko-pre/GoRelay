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
