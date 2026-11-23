package acquisition

import (
	"testing"
	"time"
)

func TestBoundedNoMatchNeedsSourceEvidence(t *testing.T) {
	pointer := "/results"
	root := ""
	cases := []struct {
		name, body string
		locator    EmptyResultEvidence
		valid      bool
	}{
		{"nested-json", `{"results":[]}`, EmptyResultEvidence{JSONPointer: &pointer}, true},
		{"root-json", `[]`, EmptyResultEvidence{JSONPointer: &root}, true},
		{"missing-json", `{"other":[]}`, EmptyResultEvidence{JSONPointer: &pointer}, false},
		{"null-json", `{"results":null}`, EmptyResultEvidence{JSONPointer: &pointer}, false},
		{"nonempty-json", `{"results":[{"title":"available"}]}`, EmptyResultEvidence{JSONPointer: &pointer}, false},
		{"empty-dom", `<main><p class="empty">No matching products</p></main>`, EmptyResultEvidence{RowsSelector: ".product", EmptySelector: ".empty", EmptyText: "No matching products"}, true},
		{"sibling-dom", `<main><section id="none">没有符合条件的活动</section></main>`, EmptyResultEvidence{RowsSelector: "article.event", EmptySelector: "#none", EmptyText: "没有符合条件的活动"}, true},
		{"wrong-selector", `<main><article class="product">available</article></main>`, EmptyResultEvidence{RowsSelector: ".invented"}, false},
		{"wrong-quote", `<p class="empty">Loading</p>`, EmptyResultEvidence{RowsSelector: ".product", EmptySelector: ".empty", EmptyText: "No results"}, false},
		{"rows-present", `<p class="empty">No results</p><article class="product">available</article>`, EmptyResultEvidence{RowsSelector: ".product", EmptySelector: ".empty", EmptyText: "No results"}, false},
		{"script-only", `<script class="empty">No results</script>`, EmptyResultEvidence{RowsSelector: ".product", EmptySelector: ".empty", EmptyText: "No results"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Response{Body: c.body, Receipt: Receipt{URL: "https://source.example/search?q=observed", Method: "GET", Status: 200, FetchedAt: time.Now()}}
			check, err := CheckEmptySource(s, c.locator)
			if (err == nil) != c.valid {
				t.Fatalf("check=%+v error=%v", check, err)
			}
			if c.valid && (len(check.Evidence) != 1 || check.Evidence[0].SourceDigest == "") {
				t.Fatal("absence lost source binding")
			}
			s.Receipt.Status = 403
			if _, err := CheckEmptySource(s, c.locator); err == nil {
				t.Fatal("failed request became no match")
			}
		})
	}
}
