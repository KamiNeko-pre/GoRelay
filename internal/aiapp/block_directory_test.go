package aiapp

import (
	"os"
	"strings"
	"testing"
)

// The directory must let a caller jump to a later headed region instead of
// paging through everything before it. On a listing page the wanted rows are
// often several thousand runes in, past an opening news region.
func TestBlockDirectoryPointsAtLaterRegions(t *testing.T) {
	path := os.Getenv("REAL_PAGE")
	if path == "" {
		t.Skip("REAL_PAGE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildHTMLInformation(string(raw), "https://bidding.swjtu.edu.cn/cggg.htm", "")
	if err != nil || info == nil {
		t.Fatal(err)
	}
	dir := blockDirectory(info)
	if len(dir) < 2 {
		t.Fatalf("directory too small to be useful: %+v", dir)
	}
	// Every region must carry a usable offset and a heading.
	foundListing := false
	for _, region := range dir {
		heading, _ := region["heading"].(string)
		start, _ := region["start_offset"].(int)
		end, _ := region["end_offset"].(int)
		if end <= start {
			t.Fatalf("region has no span: %+v", region)
		}
		if strings.Contains(heading, "采购公告") {
			foundListing = true
			t.Logf("listing region %q spans %d..%d", heading, start, end)
		}
	}
	if !foundListing {
		t.Fatalf("directory did not expose the listing region: %+v", dir)
	}
	// The listing region must be reachable by offset, i.e. inside the view.
	view := info.Text()
	total := len([]rune(view))
	for _, region := range dir {
		start, _ := region["start_offset"].(int)
		if start > total {
			t.Fatalf("region offset %d exceeds view length %d", start, total)
		}
	}
}
