package mserv

import "testing"

func TestUnquote(t *testing.T) {
	for in, want := range map[string]string{
		`"pa ss"`: "pa ss",
		`'pass'`:  "pass",
		`pass"`:   `pass"`,
		`'pass"`:  `'pass"`,
		`"`:       `"`,
		``:        ``,
	} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%s) = %s, want %s", in, got, want)
		}
	}
}
