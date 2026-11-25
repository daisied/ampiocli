package config

import (
	"encoding/json"
	"testing"
)

func TestRows(t *testing.T) {
	for reply, want := range map[string]string{
		`{"List":[{"id":1}]}`: `[{"id":1}]`,
		`{"List":[]}`:         `[]`,
		`{"List":null}`:       `[]`,
		`{}`:                  `[]`,
		`null`:                `[]`,
	} {
		got, err := rows([]byte(reply))
		if err != nil || string(got) != want {
			t.Errorf("rows(%s) = %s, %v; want %s", reply, got, err, want)
		}
	}
	// Anything else must not read as an empty table.
	for _, reply := range []string{`{"error":"busy"}`, `{"List":{"id":1}}`, `{"List":"x"}`, `[]`, `"0"`, ``} {
		if got, err := rows([]byte(reply)); err == nil {
			t.Errorf("rows(%s) = %s, want an error", reply, got)
		}
	}
}

func TestHide(t *testing.T) {
	row, err := hide(map[string]any{"id": 1, "params": jsonNumber("5")})
	if err != nil || row["params"] != jsonNumber("21") {
		t.Errorf("hide: %v, %v", row, err)
	}
	if _, err := hide(map[string]any{"id": 1}); err == nil {
		t.Error("hid a row without params")
	}
}

func jsonNumber(s string) any { return json.Number(s) }
