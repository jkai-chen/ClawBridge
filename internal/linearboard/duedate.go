package linearboard

import (
	"errors"
	"time"
)

var errDueDate = errors.New("due date must be a calendar date YYYY-MM-DD")

// ParseDueDate reads an issue due date: a real calendar date written
// YYYY-MM-DD, from 2000-01-01 to 2100-12-31.
func ParseDueDate(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !validDueDate(s) {
		return "", errDueDate
	}
	return s, nil
}

func validDueDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	t, e := time.Parse("2006-01-02", s)
	return e == nil && t.Year() >= 2000 && t.Year() <= 2100
}
