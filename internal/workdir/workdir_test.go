package workdir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	c := Copy(t.TempDir())
	icon := strings.Repeat("QUJD", 200) // 800 base64 characters
	files := Files{
		"config/objects.json": json.RawMessage(`[{"id":1,"icon":"` + icon + `"}]`),
		"nodered/a.json":      json.RawMessage(`[]`),
	}
	all := func(string) bool { return true }
	if err := c.Work().Write(files, all); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(filepath.Join(string(c), "config/objects.json"))
	if strings.Contains(string(text), icon) || !strings.Contains(string(text), `"icon": "blob:`) {
		t.Fatalf("long base64 not moved to a blob:\n%s", text)
	}
	got, err := c.Work().Read(all)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	json.Unmarshal(got["config/objects.json"], &a)
	json.Unmarshal(files["config/objects.json"], &b)
	if len(got) != 2 || !jsonEqual(a, b) {
		t.Fatalf("round trip changed the files: %s", got["config/objects.json"])
	}
}

func TestWriteReplacesOnlyOwnedFiles(t *testing.T) {
	c := Copy(t.TempDir())
	all := func(string) bool { return true }
	if err := c.Work().Write(Files{"config/a.json": json.RawMessage(`1`), "nodered/b.json": json.RawMessage(`2`)}, all); err != nil {
		t.Fatal(err)
	}
	config := func(p string) bool { return strings.HasPrefix(p, "config/") }
	if err := c.Work().Write(Files{"config/c.json": json.RawMessage(`3`)}, config); err != nil {
		t.Fatal(err)
	}
	got, err := c.Work().Read(all)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["config/a.json"]; ok || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
