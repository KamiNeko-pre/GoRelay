package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

func JSONDecode(L *lua.LState) int {
	decoder := json.NewDecoder(strings.NewReader(L.CheckString(1)))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return pushLuaError(L, fmt.Errorf("decode JSON: %w", err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return pushLuaError(L, fmt.Errorf("decode JSON: %w", err))
	}

	decoded, err := jsonValueToLua(L, value, 0)
	if err != nil {
		return pushLuaError(L, err)
	}
	L.Push(decoded)
	return 1
}

func jsonValueToLua(L *lua.LState, value any, depth int) (lua.LValue, error) {
	if depth > 32 {
		return lua.LNil, fmt.Errorf("decode JSON: value exceeds 32 nesting levels")
	}

	switch typed := value.(type) {
	case nil:
		return lua.LNil, nil
	case bool:
		return lua.LBool(typed), nil
	case string:
		return lua.LString(typed), nil
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return lua.LNil, fmt.Errorf("decode JSON number %q: %w", typed, err)
		}
		return lua.LNumber(number), nil
	case []any:
		table := L.NewTable()
		for index, item := range typed {
			converted, err := jsonValueToLua(L, item, depth+1)
			if err != nil {
				return lua.LNil, err
			}
			table.RawSetInt(index+1, converted)
		}
		return table, nil
	case map[string]any:
		table := L.NewTable()
		for key, item := range typed {
			converted, err := jsonValueToLua(L, item, depth+1)
			if err != nil {
				return lua.LNil, err
			}
			table.RawSetString(key, converted)
		}
		return table, nil
	default:
		return lua.LNil, fmt.Errorf("decode JSON: unsupported value type %T", value)
	}
}

func pushLuaError(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	return 2
}
