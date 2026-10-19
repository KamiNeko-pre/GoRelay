package engine

import (
	"github.com/yuin/gopher-lua"
	"go-lua-crawler/internal/acquisition"
)

func sourceText(l *lua.LState) int {
	text, err := acquisition.SourceText(l.CheckString(1))
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.LString(err.Error()))
		return 2
	}
	l.Push(lua.LString(text))
	l.Push(lua.LNil)
	return 2
}
