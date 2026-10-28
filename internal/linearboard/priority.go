package linearboard

import (
	"errors"
	"math"
	"strings"
)

// priorityNames are the names an issue priority may be given by, as Linear
// shows them: 0 none, 1 urgent, 2 high, 3 medium (Linear's "normal" in its
// API), 4 low.
var priorityNames = map[string]int{"none": 0, "no priority": 0, "urgent": 1, "high": 2, "medium": 3, "normal": 3, "low": 4}

var errPriority = errors.New("priority is 0-4 or a name: none, urgent, high, medium, low")

// ParsePriority reads an issue priority: Linear's whole number 0-4, as a
// number or its digit, or its name, in any case.
func ParsePriority(v any) (int, error) {
	switch x := v.(type) {
	case int:
		if x >= 0 && x <= 4 {
			return x, nil
		}
	case int64:
		if x >= 0 && x <= 4 {
			return int(x), nil
		}
	case float64:
		if x >= 0 && x <= 4 && x == math.Trunc(x) {
			return int(x), nil
		}
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		if n, ok := priorityNames[s]; ok {
			return n, nil
		}
		if len(s) == 1 && s[0] >= '0' && s[0] <= '4' {
			return int(s[0] - '0'), nil
		}
	}
	return 0, errPriority
}
