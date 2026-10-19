package aiapp

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/agentdata"
)

// Only ordinary HTTP responses reveal these randomized sites to the model.
// Gold is an evaluator input after delivery; it is never an Agent input.
func harderCrawlerCases() []hardCrawlerCase {
	const base = "http://8.8.8.8"
	salt := agentdata.Token()[:8]
	h := hardCrawlerCase{Name: "html-regional-attachment-catalog", Entry: base + "/procurement", Fields: []string{"project_id", "title", "buyer", "budget", "deadline", "region", "contact", "attachment_path", "source_url"}, Pages: map[string]string{}, Gold: map[string]map[string]string{}}
	j := hardCrawlerCase{Name: "json-multicollection-distributions", Entry: base + "/open-data", Fields: []string{"dataset_id", "title", "publisher", "license", "format", "resource_path", "updated_at", "frequency", "source_url"}, Pages: map[string]string{}, Gold: map[string]map[string]string{}}
	h.Goal = "从入口 " + h.Entry + " 遍历当前两个地区目录及所有后续分页，逐条进入采购详情；按 project_id 去重，交付全部 12 个项目。字段为 project_id（项目编号）、title（项目名称）、buyer（采购单位）、budget（预算原文）、deadline（截止时间原文）、region（地区原文）、contact（联系电话原文）、attachment_path（标为采购文件的附件 href 原文，保留相对路径及查询参数，不选更正说明）、source_url（详情地址）。规范化字段首尾及连续空白，其他值保留来源原文；不下载附件正文、不采集页面长说明。保存并实际使用可复用采集能力。"
	j.Goal = "从入口 " + j.Entry + " 发现当前两个集合，遍历每个集合的全部不透明游标分页及记录元数据，按 dataset_id 去重，交付全部 12 条。字段为 dataset_id（数据集编号）、title（名称）、publisher（机构名称，按元数据内部引用关联）、license（许可原文）、format（CSV 分发项的格式原文）、resource_path（同一 CSV 分发项的 href 原文，保留相对路径及查询参数）、updated_at（更新时间原文）、frequency（更新频率原文）、source_url（记录元数据地址）。不要混入同记录的 JSON 分发项或把机构引用键当机构名称；规范化字段空白，不转换源值，不下载数据文件。保存并实际使用可复用采集能力。"
	jsonPage := func(path string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		j.Pages[path] = string(raw)
	}
	h.Pages["/procurement"] = `<html><body><nav><a href="/help">帮助</a></nav><main><h1>当前地区采购目录</h1><section class="districts_` + salt + `"><a href="/regions/east">东部地区</a><a href="/regions/west">西部地区</a></section></main></body></html>`
	h.Pages["/help"] = `<html><body><h1>目录使用说明</h1><a href="/procurement">返回当前目录</a></body></html>`
	jsonPage("/open-data", map[string]any{"title": "开放数据目录", "collections": []any{map[string]any{"name": "环境观测", "href": "/collections/environment"}, map[string]any{"name": "公共设施", "href": "/collections/facilities"}}})
	for group, district := range []string{"east", "west"} {
		region := []string{"东部地区", "西部地区"}[group]
		collection := []string{"environment", "facilities"}[group]
		for page, indexes := range [][]int{{1, 2, 3, 4}, {4, 5, 6}} {
			path := "/regions/" + district
			apiPath := "/collections/" + collection
			cursor := fmt.Sprintf("%s-%s-next", salt, district)
			if page == 1 {
				path += "?after=" + cursor
				apiPath += "?cursor=" + cursor
			}
			var listing strings.Builder
			fmt.Fprintf(&listing, `<html><body><nav><a href="/procurement">地区目录</a></nav><main><h1>%s</h1><table class="catalog_%s"><thead><tr><th>项目</th><th>类型</th></tr></thead><tbody>`, region, salt)
			entries := []any{}
			for _, i := range indexes {
				n := group*6 + i
				fmt.Fprintf(&listing, `<tr><td><span>采购公告</span><a href="/notice/%s/%d">公共设备第%d批</a></td><td>当前</td></tr>`, salt, n, n)
				entries = append(entries, map[string]any{"label": fmt.Sprintf("第%d组观测数据", n), "links": map[string]any{"metadata": fmt.Sprintf("/records/%s/%d", salt, n)}})
			}
			listing.WriteString("</tbody></table>")
			next := ""
			if page == 0 {
				fmt.Fprintf(&listing, `<a rel="next" href="/regions/%s?after=%s">下一批</a>`, district, cursor)
				next = "/collections/" + collection + "?cursor=" + cursor
			} else {
				fmt.Fprintf(&listing, `<a rel="prev" href="/regions/%s">上一批</a>`, district)
			}
			listing.WriteString("</main></body></html>")
			h.Pages[path] = listing.String()
			jsonPage(apiPath, map[string]any{"records": entries, "links": map[string]any{"self": apiPath, "next": next}})
		}
		for i := 1; i <= 6; i++ {
			n := group*6 + i
			path := fmt.Sprintf("/notice/%s/%d", salt, n)
			attachment := fmt.Sprintf("../../files/%s/notice-%d.pdf?edition=final&lang=zh", salt, n)
			row := map[string]string{"project_id": fmt.Sprintf("CG-%s-%02d", salt, n), "title": fmt.Sprintf("公共实验设备第%d批采购", n), "buyer": fmt.Sprintf("第%d公共技术中心", n), "budget": fmt.Sprintf("%d.50万元", 17*n), "deadline": fmt.Sprintf("2026-11-%02d 17:00", n), "region": region, "contact": fmt.Sprintf("010-8100%04d", n), "attachment_path": attachment, "source_url": base + path}
			h.Gold[row["source_url"]] = row
			var details strings.Builder
			labels := []struct{ Field, Label string }{{"project_id", "项目编号"}, {"buyer", "采购单位"}, {"budget", "预算金额"}, {"deadline", "截止时间"}, {"region", "所属地区"}, {"contact", "联系电话"}}
			if n%2 == 0 {
				details.WriteString(`<dl class="metadata_` + salt + `">`)
				for _, field := range labels {
					fmt.Fprintf(&details, `<dt>%s</dt><dd data-field="%s">  %s  </dd>`, field.Label, field.Field, html.EscapeString(row[field.Field]))
				}
				details.WriteString("</dl>")
			} else {
				details.WriteString(`<table class="metadata_` + salt + `">`)
				for _, field := range labels {
					fmt.Fprintf(&details, `<tr><th>%s</th><td data-field="%s">  %s  </td></tr>`, field.Label, field.Field, html.EscapeString(row[field.Field]))
				}
				details.WriteString("</table>")
			}
			h.Pages[path] = fmt.Sprintf(`<html><body><nav><a href="/regions/%s">地区列表</a></nav><main><h2 class="title_%s">%s</h2>%s<section aria-label="附件索引"><a data-kind="amendment" href="../../files/%s/correction-%d.pdf">更正说明</a><a data-kind="procurement" href="%s">采购文件</a></section><p>%s</p></main></body></html>`, district, salt, row["title"], details.String(), salt, n, html.EscapeString(attachment), strings.Repeat("该段为采购长说明，字段和附件链接均已在独立结构中提供。", 45))
			path = fmt.Sprintf("/records/%s/%d", salt, n)
			orgKey := fmt.Sprintf("org_%s_%d", salt, n)
			resourceKey := fmt.Sprintf("csv_%s_%d", salt, n)
			row = map[string]string{"dataset_id": fmt.Sprintf("DS-%s-%02d", salt, n), "title": fmt.Sprintf("第%d组公共观测数据", n), "publisher": fmt.Sprintf("第%d开放数据研究所", n), "license": "CC-BY-4.0", "format": "CSV", "resource_path": fmt.Sprintf("../../downloads/%s/data-%d.csv?edition=2026&compressed=0", salt, n), "updated_at": fmt.Sprintf("2026-10-%02d", n), "frequency": []string{"每月", "每季度"}[n%2], "source_url": base + path}
			j.Gold[row["source_url"]] = row
			jsonPage(path, map[string]any{
				"dataset":       map[string]any{"identifier": row["dataset_id"], "title": row["title"], "publisher_ref": orgKey, "rights": map[string]any{"license": row["license"]}, "modified": row["updated_at"], "frequency": row["frequency"], "distribution_refs": []string{"json_" + resourceKey, resourceKey}},
				"organizations": map[string]any{orgKey: map[string]any{"name": row["publisher"]}},
				"distributions": map[string]any{"json_" + resourceKey: map[string]any{"format": "JSON", "href": fmt.Sprintf("../../downloads/%s/data-%d.json", salt, n)}, resourceKey: map[string]any{"format": row["format"], "href": row["resource_path"]}},
			})
		}
	}
	return []hardCrawlerCase{h, j}
}

func TestHarderCrawlerFixturesHaveReachableExactSourceFacts(t *testing.T) {
	for _, c := range harderCrawlerCases() {
		if len(c.Gold) != 12 || len(c.Fields) != 9 || len(c.Pages) < 17 {
			t.Fatal("harder difficulty contract", c.Name, len(c.Pages), len(c.Gold), len(c.Fields))
		}
		visibleText := map[string]string{}
		links := map[string][]string{}
		for path, body := range c.Pages {
			if strings.HasPrefix(body, "{") {
				var value any
				if err := json.Unmarshal([]byte(body), &value); err != nil {
					t.Fatal(err)
				}
				var walk func(any)
				walk = func(value any) {
					switch v := value.(type) {
					case string:
						visibleText[path] += v + "\n"
						links[path] = append(links[path], v)
					case map[string]any:
						for _, child := range v {
							walk(child)
						}
					case []any:
						for _, child := range v {
							walk(child)
						}
					}
				}
				walk(value)
			} else {
				visibleText[path] = html.UnescapeString(body)
				doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				doc.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
					href, _ := node.Attr("href")
					links[path] = append(links[path], href)
				})
			}
		}
		entry, _ := url.Parse(c.Entry)
		queue := []string{entry.RequestURI()}
		reached := map[string]bool{}
		for len(queue) > 0 {
			path := queue[0]
			queue = queue[1:]
			if reached[path] {
				continue
			}
			reached[path] = true
			current, _ := url.Parse("http://8.8.8.8" + path)
			for _, href := range links[path] {
				reference, err := url.Parse(href)
				if err != nil {
					continue
				}
				next := current.ResolveReference(reference)
				if _, exists := c.Pages[next.RequestURI()]; next.Hostname() == entry.Hostname() && exists {
					queue = append(queue, next.RequestURI())
				}
			}
		}
		for sourceURL, row := range c.Gold {
			parsed, err := url.Parse(sourceURL)
			if err != nil {
				t.Fatal(err)
			}
			if !reached[parsed.RequestURI()] {
				t.Fatal("gold has no reachable actual source response", sourceURL)
			}
			for _, field := range c.Fields {
				if row[field] == "" || field != "source_url" && !strings.Contains(visibleText[parsed.RequestURI()], row[field]) {
					t.Fatal("expected field is not available in exact source", c.Name, sourceURL, field)
				}
			}
		}
	}
}
