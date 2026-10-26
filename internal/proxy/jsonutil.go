package proxy

import (
	"strconv"
	"strings"
)

// jsonValue is a decoded JSON document read by key.
type jsonValue = map[string]any

// valueAt walks an object by key path.
func valueAt(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(jsonValue)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

// formatEffort renders a reasoning effort value that may be a string or number.
func formatEffort(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		return trimmed, trimmed != ""
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10), true
		}
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}
