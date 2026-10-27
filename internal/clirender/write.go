package clirender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// A View turns a command's result into a Doc. It reports false when the
// value isn't the shape it knows, and the generic layout is used instead,
// unless the view is a secret's.
type View func(v any) (Doc, bool)

var (
	viewsMu sync.RWMutex
	views   = map[string]View{}
	// secret marks views of commands that print a secret: they never fall
	// back to the generic layout (M3).
	secret = map[string]bool{}
)

// Register adds the view for a command name.
func Register(name string, v View) {
	viewsMu.Lock()
	defer viewsMu.Unlock()
	views[name] = v
}

// RegisterSecret adds the view of a command that prints a secret.
func RegisterSecret(name string, v View) {
	Register(name, v)
	viewsMu.Lock()
	defer viewsMu.Unlock()
	secret[name] = true
}

// IsSecret reports whether name is a secret's view.
func IsSecret(name string) bool {
	viewsMu.RLock()
	defer viewsMu.RUnlock()
	return secret[name]
}

// Write is every command's one exit for its result. In JSON mode it is
// exactly what the CLI wrote before: json.NewEncoder(w).Encode(v). In human
// mode it renders name's view, or the generic layout.
func Write(w io.Writer, mode Mode, name string, v any) error {
	if mode == JSON {
		return json.NewEncoder(w).Encode(v)
	}
	viewsMu.RLock()
	view, isSecret := views[name], secret[name]
	viewsMu.RUnlock()
	if view != nil {
		if doc, ok := view(v); ok {
			_, err := io.WriteString(w, Render(doc, width(w), newStyle(w)))
			return err
		}
	}
	if isSecret {
		// Never the generic layout for a secret: today's JSON instead, so it
		// still appears exactly once.
		return json.NewEncoder(w).Encode(v)
	}
	doc, err := Generic(v)
	if err != nil {
		return json.NewEncoder(w).Encode(v)
	}
	_, err = io.WriteString(w, Render(doc, width(w), newStyle(w)))
	return err
}

// ordered is a JSON object with its keys in the encoder's order: a
// struct's field order, a map's sorted keys (L1).
type ordered struct {
	keys []string
	vals map[string]any
}

// decodeOrdered reads JSON keeping each object's key order.
func decodeOrdered(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := ordered{vals: map[string]any{}}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := k.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.keys = append(o.keys, key)
				o.vals[key] = v
			}
			_, err = dec.Token()
			return o, err
		case '[':
			var list []any
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err = dec.Token()
			return list, err
		}
	}
	return t, nil
}

// Generic lays out any result the way --json would show it, for commands
// without a view yet (O-fallback): an object's fields as a block, a list of
// objects as a table, nesting deeper than one level as "(… see --json)".
// Only the top-level "source" is left out.
func Generic(v any) (Doc, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Doc{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	root, err := decodeOrdered(dec)
	if err != nil {
		return Doc{}, err
	}
	var d Doc
	o, isObj := root.(ordered)
	if !isObj {
		genericValue(&d, "", root)
		return d, nil
	}
	// The usual {source, data, …}: data first, as the body.
	if data, ok := o.vals["data"]; ok {
		genericValue(&d, "", data)
	}
	var rest []string
	for _, k := range o.keys {
		if k != "source" && k != "data" {
			rest = append(rest, k)
		}
	}
	if len(rest) > 0 {
		if _, ok := o.vals["data"]; ok {
			d.Text("")
		}
		genericObject(&d, ordered{keys: rest, vals: o.vals}, 0)
	}
	return d, nil
}

func genericValue(d *Doc, label string, v any) {
	switch x := v.(type) {
	case ordered:
		genericObject(d, x, 0)
	case []any:
		if rows, cols, ok := tableOf(x); ok {
			d.Records(cols, rows, "None.")
			return
		}
		d.KV(firstNonempty(label, "Items"), scalarList(x))
	default:
		d.Text(scalar(x))
	}
}

func genericObject(d *Doc, o ordered, depth int) {
	for _, k := range o.keys {
		switch x := o.vals[k].(type) {
		case ordered:
			if depth >= 1 {
				d.KV(Humanize(k), "(… see --json)")
				continue
			}
			d.Section(Humanize(k))
			genericObject(d, x, depth+1)
			d.End()
		case []any:
			if allScalar(x) {
				d.KV(Humanize(k), scalarList(x))
				continue
			}
			if depth >= 1 {
				d.KV(Humanize(k), fmt.Sprintf("(%d items, see --json)", len(x)))
				continue
			}
			d.Section(Humanize(k))
			if rows, cols, ok := tableOf(x); ok {
				d.Records(cols, rows, "None.")
			} else {
				d.Text(fmt.Sprintf("(%d items, see --json)", len(x)))
			}
			d.End()
		default:
			d.KV(Humanize(k), scalar(x))
		}
	}
}

// tableOf makes a table of a list of objects: the columns are every key, in
// the order first seen; a nested value shows as "(…)".
func tableOf(list []any) ([][]string, []Col, bool) {
	var keys []string
	seen := map[string]bool{}
	for _, item := range list {
		o, ok := item.(ordered)
		if !ok {
			return nil, nil, false
		}
		for _, k := range o.keys {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	cols := make([]Col, len(keys))
	for i, k := range keys {
		cols[i] = Col{Name: Humanize(k), Keep: idKey(k)}
	}
	rows := make([][]string, len(list))
	for i, item := range list {
		o := item.(ordered)
		row := make([]string, len(keys))
		for j, k := range keys {
			switch x := o.vals[k].(type) {
			case ordered, []any:
				row[j] = "(…)"
			default:
				if _, present := o.vals[k]; present {
					row[j] = scalar(x)
				}
			}
		}
		rows[i] = row
	}
	return rows, cols, true
}

// idKey is a column the next command may need whole (L4).
func idKey(k string) bool {
	return k == "id" || strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "revision") || k == "link" || k == "url"
}

func allScalar(list []any) bool {
	for _, v := range list {
		switch v.(type) {
		case ordered, []any:
			return false
		}
	}
	return true
}

func scalarList(list []any) string {
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = scalar(v)
	}
	return strings.Join(parts, ", ")
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case string:
		if x == "" {
			return "-"
		}
		return x
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

// Humanize turns a JSON key into a label: config_revision becomes "Config
// revision".
func Humanize(k string) string {
	s := strings.ReplaceAll(k, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
