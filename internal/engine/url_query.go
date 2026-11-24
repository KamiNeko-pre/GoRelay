package engine

import (
	lua "github.com/yuin/gopher-lua"
	"gorelay/internal/acquisition"
)

func URLQueryValue(L *lua.LState) int {
	value, err := acquisition.URLQueryValue(L.CheckString(1), L.CheckString(2))
	if err != nil {
		return pushLuaError(L, err)
	}
	L.Push(lua.LString(value))
	return 1
}
