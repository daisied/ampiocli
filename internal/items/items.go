// Package items is how ampiocli compares state. Every domain flattens its files
// into items, a stable key (e.g. "config/objects/110") with a JSON value, so
// diffing, conflict checks and verification are the same code everywhere.
package items

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// Items maps a key to a JSON value as decoded by Decode.
type Items map[string]any

// Change is one item that differs. Added and Removed mark a missing side.
type Change struct {
	Key            string
	Before, After  any
	Added, Removed bool
}

// Decode parses JSON keeping numbers exact (json.Number), so large flag fields survive.
func Decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}

// Diff lists the changes from before to after, in key order.
func Diff(before, after Items) []Change {
	var out []Change
	for k, b := range before {
		a, ok := after[k]
		switch {
		case !ok:
			out = append(out, Change{Key: k, Before: b, Removed: true})
		case !Equal(a, b):
			out = append(out, Change{Key: k, Before: b, After: a})
		}
	}
	for k, a := range after {
		if _, ok := before[k]; !ok {
			out = append(out, Change{Key: k, After: a, Added: true})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return keyOrder(a.Key, b.Key) })
	return out
}

// Equal compares decoded JSON values. Numbers compare by exact value, so 1.0
// equals 1: the M-SERV and editors may write the same number differently.
func Equal(a, b any) bool {
	switch a := a.(type) {
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, okA := new(big.Rat).SetString(string(a))
		y, okB := new(big.Rat).SetString(string(b))
		return okA && okB && x.Cmp(y) == 0
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if w, ok := b[k]; !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		return ok && slices.EqualFunc(a, b, Equal)
	default:
		return a == b // string, bool or nil
	}
}

// Mismatch describes, one line per field, where actual differs from the
// edit c.After. Fields the M-SERV adds of its own are not our business, so
// actual's fields count only if the edit has them or removed them (they were
// in c.Before).
func Mismatch(c Change, actual any) []string {
	a, ok1 := actual.(map[string]any)
	e, ok2 := c.After.(map[string]any)
	if ok1 && ok2 {
		b, _ := c.Before.(map[string]any)
		a = maps.Clone(a)
		maps.DeleteFunc(a, func(k string, _ any) bool {
			_, edited := e[k]
			_, removed := b[k]
			return !edited && !removed
		})
		actual = a
	}
	return Fields(c.After, actual)
}

// Describe renders a change as a header line and one line per changed field.
func Describe(c Change) []string {
	mark := "~"
	if c.Added {
		mark = "+"
	} else if c.Removed {
		mark = "-"
	}
	head := mark + " " + c.Key
	if name := label(c.After); name != "" {
		head += fmt.Sprintf("  %q", name)
	} else if name := label(c.Before); name != "" {
		head += fmt.Sprintf("  %q", name)
	}
	lines := []string{head}
	if !c.Added && !c.Removed {
		for _, l := range Fields(c.Before, c.After) {
			lines = append(lines, "    "+l)
		}
	}
	return lines
}

// Fields describes how b differs from a, one line per changed field.
func Fields(a, b any) []string { return fields(a, b, "") }

func fields(a, b any, at string) []string {
	if Equal(a, b) {
		return nil
	}
	join := func(k string) string {
		if at == "" {
			return k
		}
		return at + "." + k
	}
	switch av := a.(type) {
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			keys := maps.Clone(av)
			maps.Copy(keys, bv)
			var out []string
			for _, k := range slices.Sorted(maps.Keys(keys)) {
				out = append(out, fields(av[k], bv[k], join(k))...)
			}
			return out
		}
	case []any:
		if bv, ok := b.([]any); ok {
			var out []string
			shared := min(len(av), len(bv))
			for i := range shared {
				out = append(out, fields(av[i], bv[i], fmt.Sprintf("%s[%d]", at, i))...)
			}
			for i := shared; i < len(bv); i++ {
				out = append(out, fmt.Sprintf("%s[%d]: added %s", at, i, short(bv[i])))
			}
			for i := shared; i < len(av); i++ {
				out = append(out, fmt.Sprintf("%s[%d]: removed", at, i))
			}
			return out
		}
	}
	if at == "" {
		at = "value"
	}
	return []string{fmt.Sprintf("%s: %s → %s", at, short(a), short(b))}
}

func short(v any) string {
	if s, ok := v.(string); ok && strings.Contains(s, "\n") {
		return fmt.Sprintf("<%d lines>", strings.Count(s, "\n")+1)
	}
	if v == nil {
		return "(none)"
	}
	b, _ := json.Marshal(v)
	if r := []rune(string(b)); len(r) > 70 {
		return string(r[:67]) + "..."
	}
	return string(b)
}

// label is the name a person would recognise: Ampio rows use Polish column
// names (opis_menu = menu label), Node-RED uses name/label.
func label(v any) string {
	m, _ := v.(map[string]any)
	for _, k := range []string{"opis_menu", "opis", "name", "label", "key"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// keyOrder sorts keys segment by segment, numbers by value, so that
// config/objects/9 comes before config/objects/10.
func keyOrder(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := range min(len(as), len(bs)) {
		c := cmp.Compare(as[i], bs[i])
		if x, err := strconv.Atoi(as[i]); err == nil {
			if y, err := strconv.Atoi(bs[i]); err == nil {
				c = cmp.Compare(x, y)
			}
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}
