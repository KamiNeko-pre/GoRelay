package engine

import (
	lua "github.com/yuin/gopher-lua"
	"strings"
	"testing"
)

func TestCollectionPreservesDocumentedProgramFailure(t *testing.T) {
	for _, message := range []string{"no matching source rows", "upstream service unavailable"} {
		L := lua.NewState()
		row := L.NewTable()
		row.RawSetString("error", lua.LString(message))
		_, err := decodeCollection(row)
		L.Close()
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("program error replaced by shape error: %v", err)
		}
	}
	L := lua.NewState()
	defer L.Close()
	result := L.NewTable()
	items := L.NewTable()
	item := L.NewTable()
	item.RawSetString("error", lua.LString("a legitimate row field"))
	items.Append(item)
	result.RawSetString("items", items)
	if _, err := decodeCollection(result); err != nil {
		t.Fatal("row-level data confused with top-level failure", err)
	}
	if _, err := decodeCollection(L.NewTable()); err == nil {
		t.Fatal("malformed result accepted")
	}
}
