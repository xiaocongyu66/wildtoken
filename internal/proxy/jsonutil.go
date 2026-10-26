package proxy

import (
	"encoding/json"
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

// tokenDetailAliases are the counters gateways nest under input_token_details
// or input_tokens_details, spellings the vendor schemas do not model.
type tokenDetailAliases struct {
	CacheRead     *int64 `json:"cache_read"`
	CachedTokens  *int64 `json:"cached_tokens"`
	CacheCreation *int64 `json:"cache_creation"`
	CacheWrite    *int64 `json:"cache_write_tokens"`
}

// nestedTokenDetails decodes one nested details object the SDK kept as an
// unknown field.
func nestedTokenDetails[F interface{ Raw() string }](fields map[string]F, name string) tokenDetailAliases {
	field, present := fields[name]
	if !present {
		return tokenDetailAliases{}
	}
	raw := field.Raw()
	if raw == "" || raw == "null" {
		return tokenDetailAliases{}
	}
	var details tokenDetailAliases
	_ = json.Unmarshal([]byte(raw), &details)
	return details
}

// usageInt32Value narrows an alias count.
func usageInt32Value(value *int64) *int32 {
	if value == nil {
		return nil
	}
	return usageInt64(*value)
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
