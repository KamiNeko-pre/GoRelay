package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	lua "github.com/yuin/gopher-lua"
)

func (result luaScriptResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(result.Collection)
}

type collectionNode int

const (
	strictCollectionNode collectionNode = iota
	acquisitionCollection
	acquisitionItems
	acquisitionRow
	probeCollectionNode
)

// Only acquisition rows can carry omissions across the Lua/JSON boundary.
// A nil value preserves the field name or row slot for the host's diagnostics.
func luaJSON(value lua.LValue, seen map[*lua.LTable]bool, node collectionNode) (any, error) {
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
			childNode := strictCollectionNode
			if node == probeCollectionNode {
				childNode = probeCollectionNode
			} else if node == acquisitionCollection && key == lua.LString("items") {
				childNode = acquisitionItems
			} else if node == acquisitionItems {
				childNode = acquisitionRow
			}
			converted, err := luaJSON(child, seen, childNode)
			if err != nil {
				if node == strictCollectionNode || childNode == acquisitionItems {
					failure = err
					return
				}
				converted = nil
			}
			switch k := key.(type) {
			case lua.LString:
				object[string(k)] = converted
			case lua.LNumber:
				if float64(k) < 1 || math.Trunc(float64(k)) != float64(k) || float64(int(k)) != float64(k) {
					if node == probeCollectionNode {
						object[strconv.FormatFloat(float64(k), 'f', -1, 64)] = converted
						return
					}
					failure = fmt.Errorf("invalid collection array index")
					return
				}
				numbers[int(k)] = converted
			default:
				if node == probeCollectionNode {
					object[key.String()] = converted
					return
				}
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
			if node == probeCollectionNode {
				for idx, val := range numbers {
					object[strconv.Itoa(idx)] = val
				}
				return object, nil
			}
			return nil, fmt.Errorf("mixed array and object keys")
		}
		array := make([]any, len(numbers))
		for index := range array {
			element, ok := numbers[index+1]
			if !ok {
				if node == probeCollectionNode {
					sparse := make(map[string]any, len(numbers))
					for idx, val := range numbers {
						sparse[strconv.Itoa(idx)] = val
					}
					return sparse, nil
				}
				return nil, fmt.Errorf("sparse collection array")
			}
			array[index] = element
		}
		return array, nil
	default:
		if node == probeCollectionNode {
			return value.String(), nil
		}
		return nil, fmt.Errorf("unsupported collection value %s", value.Type())
	}
}

func decodeProbeCollection(raw lua.LValue) (map[string]any, error) {
	if table, ok := raw.(*lua.LTable); ok {
		if failure, ok := table.RawGetString("error").(lua.LString); ok && string(failure) != "" {
			return nil, fmt.Errorf("program reported failure: %s", string(failure))
		}
		value, err := luaJSON(table, map[*lua.LTable]bool{}, probeCollectionNode)
		if err != nil {
			return nil, err
		}
		if payload, ok := value.(map[string]any); ok {
			return payload, nil
		}
		return map[string]any{"result": value}, nil
	}
	value, err := luaJSON(raw, map[*lua.LTable]bool{}, probeCollectionNode)
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": value}, nil
}

func decodeCollection(table *lua.LTable, allowPartialItems bool) (map[string]any, error) {
	// Top-level error is a documented program outcome, not a malformed items
	// collection. Preserve its cause so the caller can change strategy.
	if failure, ok := table.RawGetString("error").(lua.LString); ok && string(failure) != "" {
		return nil, fmt.Errorf("program reported failure: %s", string(failure))
	}
	raw := table.RawGetString("items")
	items, ok := raw.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("collection items must be an array, got %s. Return true,{items={{field=value,...},{...}}} — a Lua sequence of row tables, not a map and not a single row", raw.Type())
	}
	node := strictCollectionNode
	if allowPartialItems {
		node = acquisitionCollection
	}
	value, err := luaJSON(table, map[*lua.LTable]bool{}, node)
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
