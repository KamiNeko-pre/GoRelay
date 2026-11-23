package ruleconfig

import (
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

type Config struct {
	TitleSelector string `json:"title_selector"`
	BodySelector  string `json:"body_selector"`
}

type Extracted struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Extract executes declarative selectors with a trusted parser. No generated
// script is evaluated. Missing or ambiguous fields fail before import.
func Extract(rawHTML string, config Config) (Extracted, error) {
	if len(rawHTML) > 4<<20 {
		return Extracted{}, fmt.Errorf("HTML exceeds 4 MiB")
	}
	selectors := []string{config.TitleSelector, config.BodySelector}
	matchers := make([]cascadia.Selector, len(selectors))
	for index, selector := range selectors {
		if strings.TrimSpace(selector) == "" || len(selector) > 512 {
			return Extracted{}, fmt.Errorf("selector %d must contain 1–512 bytes", index)
		}
		matcher, err := cascadia.Compile(selector)
		if err != nil {
			return Extracted{}, fmt.Errorf("selector %d: %w", index, err)
		}
		matchers[index] = matcher
	}
	dom, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return Extracted{}, fmt.Errorf("parse HTML: %w", err)
	}
	fields := make([]string, 2)
	for index, matcher := range matchers {
		selected := dom.FindMatcher(matcher)
		if selected.Length() != 1 {
			return Extracted{}, fmt.Errorf("field %d expected one element, got %d", index, selected.Length())
		}
		fields[index] = visibleText(selected.Get(0))
		if fields[index] == "" {
			return Extracted{}, fmt.Errorf("field %d is empty", index)
		}
	}
	return Extracted{Title: fields[0], Body: fields[1]}, nil
}

func visibleText(root *html.Node) string {
	var output strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.TextNode {
			output.WriteString(node.Data)
			return
		}
		if node.Type != html.ElementNode {
			return
		}
		switch node.Data {
		case "script", "style", "template", "nav", "aside", "footer":
			return
		}
		block := false
		switch node.Data {
		case "article", "section", "div", "p", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "li", "br", "tr", "blockquote":
			block = true
		}
		if block {
			output.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
		if block {
			output.WriteByte('\n')
		}
	}
	visit(root)
	return strings.TrimSpace(output.String())
}
