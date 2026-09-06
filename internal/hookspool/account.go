package hookspool

import (
	"encoding/json"
	"sort"
	"strings"
)

// AccountIn tolerates both the context/data envelope used by
// Standardized Hooks and the slightly different return shapes of WHM API
// account functions. Only a cPanel-safe username is accepted.
func AccountIn(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(value any) string {
		switch typed := value.(type) {
		case map[string]any:
			if data, ok := typed["data"]; ok {
				if found := walk(data); found != "" {
					return found
				}
			}
			for _, key := range []string{"user", "username", "cpuser"} {
				if candidate, ok := typed[key].(string); ok && safeCPanelUser(candidate) {
					return candidate
				}
			}
			// Sorted, because Go randomises map iteration and this
			// answer decides which account a blocking removal hook
			// evaluates. An envelope with a usable name in more than one
			// branch would otherwise name a different account from one
			// run to the next.
			keys := make([]string, 0, len(typed))
			for key := range typed {
				if key != "data" {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				if found := walk(typed[key]); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

func safeCPanelUser(value string) bool {
	if value == "" || len(value) > 16 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return !strings.HasPrefix(value, "_")
}
