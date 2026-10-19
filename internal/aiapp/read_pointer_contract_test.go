package aiapp

import "testing"

func TestJSONPointerArrayIndicesCannotHaveInfiniteAliases(t *testing.T) {
	value:=map[string]any{"items":[]any{"observed"},"00":"literal-object-key"}
	for _, pointer:=range []string{"/items/00","/items/+0","/items/000000"} {
		if _,err:=jsonPointer(value,pointer);err==nil {t.Fatal("noncanonical array index accepted",pointer)}
	}
	for _,pointer:=range []string{"/items/0","/00"} {
		if _,err:=jsonPointer(value,pointer);err!=nil {t.Fatal("valid array index or literal object key rejected",pointer,err)}
	}
}
