package mcp

// Small exported JSON-Schema builders used to describe tool inputs.

// Obj builds an object schema with the given properties and required keys.
func Obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Str is a string property.
func Str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// StrEnum is a string property constrained to a set of values.
func StrEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// Bool is a boolean property.
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Int is an integer property.
func Int(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Num is a number property.
func Num(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

// Arr is an array property with the given item schema.
func Arr(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}
