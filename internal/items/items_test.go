package items

import (
	"slices"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	v, err := Decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEqualComparesNumbersByValue(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`1.0`, `1`, true},
		{`{"x":[1e2]}`, `{"x":[100]}`, true},
		{`18446744073709551617`, `18446744073709551616`, false}, // beyond float64
		{`1`, `"1"`, false},
		{`{"x":1}`, `{"x":1,"y":null}`, false},
	} {
		if got := Equal(decode(t, c.a), decode(t, c.b)); got != c.want {
			t.Errorf("Equal(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestDiffAndDescribe(t *testing.T) {
	before := Items{"t/9": decode(t, `{"name":"a","n":[1,2]}`), "t/10": decode(t, `{"name":"gone"}`)}
	after := Items{"t/9": decode(t, `{"name":"a","n":[1,3,4]}`), "t/11": decode(t, `{"name":"new"}`)}
	changes := Diff(before, after)
	var lines []string
	for _, c := range changes {
		lines = append(lines, Describe(c)...)
	}
	want := []string{
		`~ t/9  "a"`,
		`    n[1]: 2 → 3`,
		`    n[2]: added 4`,
		`- t/10  "gone"`,
		`+ t/11  "new"`,
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got\n%q\nwant\n%q", lines, want)
	}
}

func TestMismatchIgnoresExtraFields(t *testing.T) {
	actual := decode(t, `{"id":1,"name":"a","stamp":5}`)
	if m := Mismatch(actual, decode(t, `{"id":1,"name":"a"}`)); m != nil {
		t.Errorf("extra field counted: %q", m)
	}
	if m := Mismatch(actual, decode(t, `{"id":1,"name":"b"}`)); !slices.Equal(m, []string{`name: "b" → "a"`}) {
		t.Errorf("got %q", m)
	}
}
