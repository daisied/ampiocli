package workdir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daisied/ampiocli/internal/items"
)

func TestWriteReadRoundTrip(t *testing.T) {
	c := Copy(t.TempDir())
	icon := strings.Repeat("QUJD", 200) // 800 base64 characters
	files := Files{
		"config/objects.json": json.RawMessage(`[{"id":1,"icon":"` + icon + `"}]`),
		"nodered/a.json":      json.RawMessage(`[]`),
	}
	if err := c.Work().Write(files, All); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(string(c), "config/objects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), icon) || !strings.Contains(string(text), `"icon": "blob:`) {
		t.Fatalf("long base64 not moved to a blob:\n%s", text)
	}
	got, err := c.Work().Read(All)
	if err != nil {
		t.Fatal(err)
	}
	a, errA := items.Decode(got["config/objects.json"])
	b, errB := items.Decode(files["config/objects.json"])
	if errA != nil || errB != nil || len(got) != 2 || !items.Equal(a, b) {
		t.Fatalf("round trip changed the files: %s", got["config/objects.json"])
	}
}

func TestWriteReplacesOnlyOwnedFiles(t *testing.T) {
	c := Copy(t.TempDir())
	if err := c.Work().Write(Files{"config/a.json": json.RawMessage(`1`), "nodered/b.json": json.RawMessage(`2`)}, All); err != nil {
		t.Fatal(err)
	}
	config := func(p string) bool { return strings.HasPrefix(p, "config/") }
	if err := c.Work().Write(Files{"config/c.json": json.RawMessage(`3`)}, config); err != nil {
		t.Fatal(err)
	}
	got, err := c.Work().Read(All)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["config/a.json"]; ok || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
