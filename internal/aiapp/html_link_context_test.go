package aiapp

import (
	"strings"
	"testing"
)

// A listing row must expose its own target beside its own title, and site chrome
// must stay separate. Before v2 every anchor was swept into one flat tail, so a
// reader could see titles and see hrefs but could not join them.
//
// The fixture mirrors the real source: no <main>, so the projected region is the
// whole body and site chrome is in range and must therefore be classified.
func TestHTMLInformationAttachesListingTargetToItsOwnRow(t *testing.T) {
	body := `<html><body>
<header class="site-header"><a href="/index.htm">首页</a></header>
<div class="wrapper">
  <ul class="cggg-list">
    <li><a href="info/1016/72587.htm">教学数据资源平台公告</a></li>
    <li><a href="info/1016/67057.htm">数字孪生项目公告</a></li>
  </ul>
</div>
<footer class="site-footer"><a href="/map.htm">网站地图</a></footer>
</body></html>`
	info, err := buildHTMLInformation(body, "https://bidding.example.edu.cn/cggg.htm", "")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Fatal("HTML projection unavailable")
	}
	if info.Version != htmlInformationVersion {
		t.Fatalf("representation version not advanced: %s", info.Version)
	}

	// Each list item carries its own resolved absolute target.
	want := map[string]string{
		"教学数据资源平台公告": "https://bidding.example.edu.cn/info/1016/72587.htm",
		"数字孪生项目公告":   "https://bidding.example.edu.cn/info/1016/67057.htm",
	}
	got := map[string]string{}
	for _, block := range info.Blocks {
		for _, link := range block.Links {
			if _, ok := want[link.Text]; ok && block.Kind == "li" {
				got[link.Text] = link.Href
			}
		}
	}
	for text, href := range want {
		if got[text] != href {
			t.Fatalf("row %q did not carry its own target: got %q want %q", text, got[text], href)
		}
	}

	// Chrome anchors must not be attached to a listing row.
	for _, block := range info.Blocks {
		if block.Kind != "li" {
			continue
		}
		for _, link := range block.Links {
			if strings.Contains(link.Href, "map.htm") || link.Href == "https://bidding.example.edu.cn/index.htm" {
				t.Fatalf("chrome anchor %q leaked into a listing row", link.Href)
			}
		}
	}

	// Chrome remains readable and labelled as its own class.
	classes := map[string]bool{}
	for _, block := range info.Blocks {
		if block.Kind == "link" && block.Class != "" {
			classes[block.Class] = true
		}
	}
	if !classes["footer"] || !classes["header"] {
		t.Fatalf("chrome anchors were not labelled separately: %+v", classes)
	}

	// The rendered text keeps the pairing adjacent for the model.
	text := info.Text()
	if !strings.Contains(text, "教学数据资源平台公告 → https://bidding.example.edu.cn/info/1016/72587.htm") {
		t.Fatalf("rendered text lost the title-to-target pairing:\n%s", text)
	}
}

// A table row's link belongs to that row, not to the page tail.
func TestHTMLInformationAttachesTableRowTarget(t *testing.T) {
	body := `<main><table>
<tr><th>项目</th><th>公告</th></tr>
<tr><td>三维建模</td><td><a href="/notice/128.htm">采购邀请</a></td></tr>
</table></main>`
	info, err := buildHTMLInformation(body, "https://bidding.example.edu.cn/list.htm", "")
	if err != nil || info == nil {
		t.Fatal(err)
	}
	found := ""
	for _, block := range info.Blocks {
		if block.Kind != "table_row" {
			continue
		}
		for _, link := range block.Links {
			if link.Href == "https://bidding.example.edu.cn/notice/128.htm" {
				found = block.Text
			}
		}
	}
	if found == "" || !strings.Contains(found, "采购邀请") {
		t.Fatalf("table row did not own its target: %+v", info.Blocks)
	}
}
