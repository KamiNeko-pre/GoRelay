package engine

import (
	"encoding/json"
	"fmt"
	"math"

	lua "github.com/yuin/gopher-lua"
)

func (result luaScriptResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(result.Collection)
}

// Copy values while the owning VM is alive. Reject values JSON cannot represent.
func luaJSON(value lua.LValue, seen map[*lua.LTable]bool, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("collection exceeds 32 nesting levels")
	}
	switch v := value.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(v), nil
	case lua.LString:
		return string(v), nil
	case lua.LNumber:
		n := float64(v)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("non-finite collection number")
		}
		return n, nil
	case *lua.LTable:
		if seen[v] {
			return nil, fmt.Errorf("cyclic collection table")
		}
		seen[v] = true
		defer delete(seen, v)
		object := map[string]any{}
		numbers := map[int]any{}
		var failure error
		v.ForEach(func(key, child lua.LValue) {
			if failure != nil {
				return
			}
			converted, err := luaJSON(child, seen, depth+1)
			if err != nil {
				failure = err
				return
			}
			switch k := key.(type) {
			case lua.LString:
				object[string(k)] = converted
			case lua.LNumber:
				if float64(k) < 1 || float64(k) > 100000 || math.Trunc(float64(k)) != float64(k) {
					failure = fmt.Errorf("invalid collection array index")
					return
				}
				numbers[int(k)] = converted
			default:
				failure = fmt.Errorf("unsupported collection key type %s", key.Type())
			}
		})
		if failure != nil {
			return nil, failure
		}
		if len(numbers) == 0 {
			return object, nil
		}
		if len(object) > 0 {
			return nil, fmt.Errorf("mixed array and object keys")
		}
		array := make([]any, len(numbers))
		for index := range array {
			element, ok := numbers[index+1]
			if !ok {
				return nil, fmt.Errorf("sparse collection array")
			}
			array[index] = element
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unsupported collection value %s", value.Type())
	}
}

func decodeCollection(table *lua.LTable) (map[string]any, error) {
	// Top-level error is a documented program outcome, not a malformed items
	// collection. Preserve its cause so the caller can change strategy.
	if failure, ok := table.RawGetString("error").(lua.LString); ok && string(failure) != "" {
		return nil, fmt.Errorf("program reported failure: %s", string(failure))
	}
	raw := table.RawGetString("items")
	if _, ok := raw.(*lua.LTable); !ok {
		return nil, fmt.Errorf("collection items must be an array, got %s. Return true,{items={{field=value,...},{...}}} — a Lua sequence of row tables, not a map and not a single row", raw.Type())
	}
	items, ok := raw.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("collection items must be an array")
	}
	value, err := luaJSON(table, map[*lua.LTable]bool{}, 0)
	if err != nil {
		return nil, err
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("collection must be an object")
	}
	if items.Len() == 0 {
		if object, ok := payload["items"].(map[string]any); ok && len(object) == 0 {
			payload["items"] = []any{}
		}
	}
	if _, ok := payload["items"].([]any); !ok {
		return nil, fmt.Errorf("collection items must use sequential numeric keys")
	}
	return payload, nil
}
