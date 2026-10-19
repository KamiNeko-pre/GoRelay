package aiapp

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// decodeArgs runs before tool execution. Preserve the typed cause for callers,
// while making the parser boundary explicit to recovery and the model.
type ArgumentDecodeError struct {
	Reason      string `json:"reason"`
	Field       string `json:"field,omitempty"`
	Expected    string `json:"expected,omitempty"`
	Actual      string `json:"actual,omitempty"`
	Offset      int64  `json:"byte_offset,omitempty"`
	LimitBytes  int    `json:"limit_bytes,omitempty"`
	ActualBytes int    `json:"actual_bytes,omitempty"`
	cause       error
}

func (e *ArgumentDecodeError) Error() string { return e.Reason }
func (e *ArgumentDecodeError) Unwrap() error { return e.cause }

func argumentDecodeFailure(err error, offset int64) *ArgumentDecodeError {
	f := &ArgumentDecodeError{Reason: err.Error(), Offset: offset, cause: err}
	var mismatch *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	if errors.As(err, &mismatch) {
		f.Field, f.Expected, f.Actual, f.Offset = mismatch.Field, mismatch.Type.String(), mismatch.Value, mismatch.Offset
	} else if errors.As(err, &syntax) {
		f.Offset = syntax.Offset
	}
	return f
}

// Follow the JSON grammar until the first parser error. A byte offset alone
// cannot tell a model which schema value to correct, especially when the
// gateway mislabels a syntactically incomplete call as output length.
func malformedArgumentPath(raw string) string {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	invalid := ""
	var value func(string) error
	value = func(path string) error {
		token, err := d.Token()
		if err != nil {
			invalid = path
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			for d.More() {
				key, err := d.Token()
				if err != nil {
					invalid = path
					return err
				}
				name, ok := key.(string)
				if !ok {
					invalid = path
					return errors.New("object key is not a string")
				}
				next := pointerKey(name)
				if path != "" {
					next = path + "/" + next
				}
				if err = value(next); err != nil {
					return err
				}
			}
		case '[':
			for n := 0; d.More(); n++ {
				next := strconv.Itoa(n)
				if path != "" {
					next = path + "/" + next
				}
				if err = value(next); err != nil {
					return err
				}
			}
		}
		_, err = d.Token()
		if err != nil {
			invalid = path
		}
		return err
	}
	_ = value("")
	return invalid
}
func argumentSchemaExpected(request ChatRequest, tool, path string) string {
	var schema map[string]any
	for _, def := range request.Tools {
		if def.Function.Name == tool {
			_ = json.Unmarshal(def.Function.Parameters, &schema)
			break
		}
	}
	if schema == nil || path == "" {
		return "JSON object"
	}
	for _, part := range strings.Split(path, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		if schema["type"] == "array" {
			schema, _ = schema["items"].(map[string]any)
		} else {
			props, _ := schema["properties"].(map[string]any)
			schema, _ = props[part].(map[string]any)
		}
		if schema == nil {
			return "valid JSON value matching the offered schema"
		}
	}
	typ, _ := schema["type"].(string)
	if typ == "array" {
		items, _ := schema["items"].(map[string]any)
		item, _ := items["type"].(string)
		if item != "" {
			return "array of " + item
		}
		return "array"
	}
	if typ == "" {
		return "valid JSON value matching the offered schema"
	}
	return typ
}
