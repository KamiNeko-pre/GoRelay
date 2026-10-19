package aiapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// v2 attaches each link to the block that contains it, so a list item's own
// target is readable beside its own title. v1 swept every anchor into a flat
// tail, which left a title and its href in two unmatchable projections. Old
// cursors must restart: offsets belong to the named representation.
const htmlInformationVersion = "html-information-v2"

// A rebuildable reading representation. Selectors locate original nodes; neither
// extracted text nor an embedded JSON sample is an independently verified fact.
type HTMLInformation struct {
	Version     string             `json:"version"`
	Blocks      []HTMLContentBlock `json:"blocks"`
	Embedded    []HTMLEmbeddedJSON `json:"embedded,omitempty"`
	Matches     int                `json:"matches"`
	TotalBlocks int                `json:"total_blocks"`
	NextBlock   *int               `json:"next_block,omitempty"`
}

type HTMLContentBlock struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	Selector      string         `json:"selector"`
	Context       []string       `json:"context,omitempty"`
	Text          string         `json:"text"`
	Cells         []HTMLCell     `json:"cells,omitempty"`
	Headers       []string       `json:"headers,omitempty"`
	Caption       string         `json:"caption,omitempty"`
	TextTruncated bool           `json:"text_truncated,omitempty"`
	TotalRunes    int            `json:"total_runes,omitempty"`
	ReadWith      map[string]any `json:"read_with,omitempty"`
	// Links are the anchors this block contains, so a listing row's own target is
	// readable beside its own title. Class labels chrome on standalone link
	// blocks; it is empty for content-attached links.
	Links []HTMLLink `json:"links,omitempty"`
	Class string     `json:"class,omitempty"`
}

// HTMLLink is one resolved anchor: visible text plus the absolute target URL.
type HTMLLink struct {
	Text string `json:"text"`
	Href string `json:"href"`
}

type HTMLCell struct {
	Text    string `json:"text"`
	Colspan string `json:"colspan,omitempty"`
	Rowspan string `json:"rowspan,omitempty"`
}

type HTMLEmbeddedJSON struct {
	ID        string          `json:"id"`
	Selector  string          `json:"selector"`
	Structure *JSONInspection `json:"structure"`
	Value     json.RawMessage `json:"-"` // Original bytes remain in Source.Body.
	ReadWith  map[string]any  `json:"read_with,omitempty"`
}

// Normal content is read before links, code samples and embedded-data metadata.
// Moving samples changes presentation, never the original DOM or their locators.
// A content block renders its own anchors inline so a listing row's target is
// readable beside its own title; chrome anchors stay in the trailing group with
// their class label, and remain selectable by block_id.
func (v *HTMLInformation) Text() string {
	var text strings.Builder
	for _, kind := range []string{"content", "link", "code", "embedded_json"} {
		for _, block := range v.Blocks {
			group := block.Kind
			if group != "link" && group != "code" && group != "embedded_json" {
				group = "content"
			}
			if group != kind {
				continue
			}
			fmt.Fprintf(&text, "[%s %s]", block.Kind, block.ID)
			if len(block.Context) > 0 {
				text.WriteString(" " + strings.Join(block.Context, " > "))
			}
			if block.Class != "" {
				text.WriteString(" (" + block.Class + ")")
			}
			text.WriteString("\n" + block.Text)
			for _, link := range block.Links {
				text.WriteString("\n" + link.Text + " → " + link.Href)
			}
			text.WriteString("\n\n")
		}
	}
	return text.String()
}

func htmlNoise(node *html.Node) bool {
	if node.Type != html.ElementNode {
		return false
	}
	switch node.Data {
	case "script", "style", "svg", "canvas", "iframe", "nav":
		return true
	}
	for _, attr := range node.Attr {
		if (attr.Key == "role" && attr.Val == "navigation") || (attr.Key == "aria-hidden" && attr.Val == "true") || attr.Key == "hidden" {
			return true
		}
	}
	return false
}

func visibleHTMLText(node *html.Node) string {
	var text strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n == nil || htmlNoise(n) {
			return
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
			if child.Type == html.ElementNode && (htmlBlockElement(child.Data) || child.Data == "br") {
				text.WriteByte(' ')
			}
		}
	}
	visit(node)
	return strings.Join(strings.Fields(text.String()), " ")
}

func htmlBlockElement(tag string) bool {
	switch tag {
	case "html", "body", "main", "article", "section", "div", "header", "footer", "aside", "p", "ul", "ol", "li", "dl", "dt", "dd", "blockquote", "figure", "figcaption", "table", "pre", "h1", "h2", "h3", "h4", "h5", "h6":
		return true
	}
	return false
}

func buildHTMLInformation(body, sourceURL, selector string, specs ...*TaskSpec) (*HTMLInformation, error) {
	if json.Valid([]byte(body)) {
		return nil, nil // Markup in a JSON string is not the response DOM.
	}
	parser := html.NewTokenizer(strings.NewReader(body))
	markup := false
	for !markup {
		switch parser.Next() {
		case html.StartTagToken, html.SelfClosingTagToken, html.DoctypeToken:
			markup = true
		case html.ErrorToken:
			if parser.Err() != io.EOF {
				return nil, parser.Err()
			}
			return nil, nil
		}
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	roots := doc.Find("main, article, [role=main]")
	if selector != "" {
		roots = doc.Find(selector)
		if roots.Length() == 0 {
			return nil, fmt.Errorf("selector matches no elements in persisted source")
		}
	} else if roots.Length() == 0 {
		roots = doc.Find("body")
	}
	out := &HTMLInformation{Version: htmlInformationVersion, Blocks: []HTMLContentBlock{}, Matches: roots.Length()}
	headings := []string{}
	// nodeOwner maps an emitted source node to its block index so a later anchor
	// pass can attach each link to the block that already contains it.
	nodeOwner := map[*html.Node]int{}
	appendBlock := func(kind string, node *html.Node, text string, cells []HTMLCell) {
		if strings.TrimSpace(text) == "" {
			return
		}
		context := []string{}
		for _, heading := range headings {
			if heading != "" {
				context = append(context, heading)
			}
		}
		nodeOwner[node] = len(out.Blocks)
		out.Blocks = append(out.Blocks, HTMLContentBlock{ID: "b" + strconv.Itoa(len(out.Blocks)), Kind: kind, Selector: domSelector(node), Context: context, Text: text, Cells: cells})
	}
	// Extract literal JSON before discarding executable/noisy script nodes. No
	// JavaScript evaluation, assignment stripping or provider-name special case.
	scripts := doc.Find("script")
	if selector != "" {
		scripts = roots.Find("script").AddSelection(roots.Filter("script"))
	}
	scripts.Each(func(_ int, script *goquery.Selection) {
		raw := strings.TrimSpace(script.Text())
		structure := inspectJSON(raw, specs...)
		if structure == nil {
			return
		}
		id := "j" + strconv.Itoa(len(out.Embedded))
		out.Embedded = append(out.Embedded, HTMLEmbeddedJSON{ID: id, Selector: domSelector(script.Nodes[0]), Structure: structure, Value: json.RawMessage(raw)})
		appendBlock("embedded_json", script.Nodes[0], "Literal JSON "+id+" ("+structure.Root+"); read this node as structured data, not executable code.", nil)
	})
	if selector == "" {
		if title := doc.Find("head > title").First(); title.Length() > 0 {
			appendBlock("title", title.Nodes[0], visibleHTMLText(title.Nodes[0]), nil)
		}
	}
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if htmlNoise(node) || node.Type != html.ElementNode {
			return
		}
		if len(node.Data) == 2 && node.Data[0] == 'h' && node.Data[1] >= '1' && node.Data[1] <= '6' {
			level := int(node.Data[1] - '1')
			text := visibleHTMLText(node)
			for len(headings) <= level {
				headings = append(headings, "")
			}
			headings = append(headings[:level], text)
			appendBlock("heading", node, text, nil)
			return
		}
		switch node.Data {
		case "pre":
			// Text() preserves indentation/newlines without serializing HTML.
			appendBlock("code", node, strings.TrimSpace(goquery.NewDocumentFromNode(node).Text()), nil)
			return
		case "p", "li", "dt", "dd", "blockquote", "figcaption":
			appendBlock(node.Data, node, visibleHTMLText(node), nil)
			return
		case "table":
			table := goquery.NewDocumentFromNode(node).Selection
			caption := ""
			table.Find("caption").First().Each(func(_ int, part *goquery.Selection) {
				caption = visibleHTMLText(part.Nodes[0])
			})
			headers := []string{}
			table.Find("tr").Each(func(_ int, row *goquery.Selection) {
				if row.Closest("table").Get(0) != node {
					return // Nested tables have their own layout, not outer columns.
				}
				cells := []HTMLCell{}
				values := []string{}
				row.ChildrenFiltered("td, th").Each(func(_ int, cell *goquery.Selection) {
					v := HTMLCell{Text: visibleHTMLText(cell.Nodes[0])}
					v.Colspan, _ = cell.Attr("colspan")
					v.Rowspan, _ = cell.Attr("rowspan")
					cells = append(cells, v)
					value := v.Text
					if v.Colspan != "" || v.Rowspan != "" {
						value += fmt.Sprintf(" [colspan=%s rowspan=%s]", v.Colspan, v.Rowspan)
					}
					values = append(values, value)
				})
				line := strings.Join(values, " | ")
				if row.ChildrenFiltered("td").Length() == 0 {
					headers = append(headers, line)
					return
				}
				text := ""
				if caption != "" {
					text += caption + "\n"
				}
				if len(headers) > 0 {
					text += "Headers: " + strings.Join(headers, " / ") + "\n"
				}
				appendBlock("table_row", row.Nodes[0], text+line, cells)
				if len(out.Blocks) > 0 && out.Blocks[len(out.Blocks)-1].Kind == "table_row" {
					last := &out.Blocks[len(out.Blocks)-1]
					last.Headers = append([]string{}, headers...)
					last.Caption = caption
				}
			})
			if len(headers) > 0 && table.Find("td").Length() == 0 {
				appendBlock("table_header", node, caption+"\n"+strings.Join(headers, " / "), nil)
			}
			return
		}
		var inline strings.Builder
		flush := func() {
			appendBlock("text", node, strings.Join(strings.Fields(inline.String()), " "), nil)
			inline.Reset()
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && htmlBlockElement(child.Data) {
				flush()
				visit(child)
			} else {
				inline.WriteString(visibleHTMLText(child))
				inline.WriteByte(' ')
			}
		}
		flush()
	}
	selected := map[*html.Node]bool{}
	for _, node := range roots.Nodes {
		selected[node] = true
	}
	roots.Each(func(_ int, root *goquery.Selection) {
		node := root.Nodes[0]
		for parent := node.Parent; parent != nil; parent = parent.Parent {
			if selected[parent] {
				return // main/article nesting must not duplicate records or text.
			}
		}
		visit(node)
	})
	base, _ := url.Parse(sourceURL)
	headings = nil // Links are node references, not facts under the final heading.
	seenLinks := map[string]bool{}
	roots.Find("a[href]").AddSelection(roots.Filter("a[href]")).Each(func(_ int, link *goquery.Selection) {
		for node := link.Nodes[0]; node != nil; node = node.Parent {
			if htmlNoise(node) {
				return
			}
		}
		href, _ := link.Attr("href")
		u, err := url.Parse(href)
		if err != nil || base == nil {
			return
		}
		u = base.ResolveReference(u)
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		text := visibleHTMLText(link.Nodes[0])
		if text == "" {
			return
		}
		key := text + " → " + u.String()
		if seenLinks[key] {
			return
		}
		seenLinks[key] = true
		// Attach the anchor to the content block that already contains it, so a
		// listing row's own target reads beside its own title. Chrome anchors are
		// grouped and labelled separately instead of being swept into a flat tail.
		class := classifyLinkContext(link.Nodes[0])
		if class == "content" {
			if owner := containingBlock(nodeOwner, link.Nodes[0]); owner >= 0 {
				out.Blocks[owner].Links = append(out.Blocks[owner].Links, HTMLLink{Text: text, Href: u.String()})
				return
			}
		}
		out.Blocks = append(out.Blocks, HTMLContentBlock{
			ID: "b" + strconv.Itoa(len(out.Blocks)), Kind: "link", Selector: domSelector(link.Nodes[0]),
			Text: key, Class: class, Links: []HTMLLink{{Text: text, Href: u.String()}},
		})
	})
	out.TotalBlocks = len(out.Blocks)
	return out, nil
}

// containingBlock finds the nearest already-emitted block that owns this node.
func containingBlock(nodeOwner map[*html.Node]int, node *html.Node) int {
	for n := node.Parent; n != nil; n = n.Parent {
		if at, ok := nodeOwner[n]; ok {
			return at
		}
	}
	return -1
}

// classifyLinkContext separates a listing row from site chrome by ancestor
// semantics, never by hostname or page-specific spelling.
//
// Only markers that reliably denote chrome are used. A bare "sidebar" is not one
// of them: real listing pages ship main content inside containers such as
// "main-contentleft left-sidebar", so matching it would demote the actual rows.
func classifyLinkContext(node *html.Node) string {
	for n := node; n != nil; n = n.Parent {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.Data {
		case "nav":
			return "nav"
		case "header":
			return "header"
		case "footer":
			return "footer"
		}
		for _, attr := range n.Attr {
			if attr.Key == "role" {
				switch attr.Val {
				case "navigation":
					return "nav"
				case "banner":
					return "header"
				case "contentinfo":
					return "footer"
				}
			}
			if attr.Key != "class" && attr.Key != "id" {
				continue
			}
			lower := strings.ToLower(attr.Val)
			switch {
			case strings.Contains(lower, "breadcrumb"), strings.Contains(lower, "copyright"):
				return "nav"
			case strings.Contains(lower, "nav"):
				return "nav"
			case strings.Contains(lower, "header"), strings.Contains(lower, "topbar"):
				return "header"
			case strings.Contains(lower, "footer"):
				return "footer"
			}
		}
	}
	return "content"
}
