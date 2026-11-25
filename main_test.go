package main

// End-to-end tests: the real CLI against the simulator, seeded with
// testdata/house, and a miniature Designer bundle (testdata/designer) with the
// same shapes as the real one. Its toy codec stores a rule {"value": n} as
// the byte n and refuses values over 200; a rule {"value": n, "link": l} is
// the bytes 201, n, with l in the module's linked table.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daisied/ampiocli/internal/sim"
	"github.com/daisied/ampiocli/internal/workdir"
)

// house starts a simulator and points new working copies at it.
func house(t *testing.T) *sim.Sim {
	t.Helper()
	cache := t.TempDir() // keep Designer downloads out of the real cache
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	files, err := workdir.Copy("testdata/house").Work().Read(func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.Start(files, "testdata/designer", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	t.Setenv("AMPIO_MQTT_URL", s.Settings.MQTT)
	t.Setenv("AMPIO_DESIGNER_URL", s.Settings.Designer)
	t.Setenv("AMPIO_NODERED_URL", s.Settings.NodeRED)
	t.Setenv("AMPIO_PASSWORD", s.Settings.Password)
	return s
}

// wc is a working copy in a temporary folder.
type wc struct {
	t   *testing.T
	dir string
}

func newCopy(t *testing.T) wc { return wc{t, t.TempDir()} }

func (c wc) run(args ...string) (string, error) {
	var out strings.Builder
	err := run(append([]string{"-C", c.dir}, args...), &out)
	return out.String(), err
}

func (c wc) must(args ...string) string {
	c.t.Helper()
	out, err := c.run(args...)
	if err != nil {
		c.t.Fatalf("ampiocli %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (c wc) read(file string) string {
	c.t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.dir, file))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(raw)
}

// edit changes one JSON file of the working copy.
func edit[T any](c wc, file string, change func(*T)) {
	c.t.Helper()
	var v T
	if err := json.Unmarshal([]byte(c.read(file)), &v); err != nil {
		c.t.Fatal(err)
	}
	change(&v)
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, file), raw, 0o644); err != nil {
		c.t.Fatal(err)
	}
}

type rows = []map[string]any

func TestPush(t *testing.T) {
	house(t)
	me, check := newCopy(t), newCopy(t)
	me.must("pull")
	if !strings.Contains(me.read("logic/10a.json"), `"link": 3`) {
		t.Fatalf("module 10a not decoded with its linked table:\n%s", me.read("logic/10a.json"))
	}
	if _, err := os.Stat(filepath.Join(me.dir, "logic/20b.json")); err == nil {
		t.Fatal("a protocol 26 module was decoded")
	}

	edit(me, "config/objects.json", func(r *rows) {
		(*r)[0]["opis_menu"] = "Hall lights"
		*r = append(*r,
			map[string]any{"id": 3, "opis_menu": "Fan", "params": 0},
			map[string]any{"id": "@lamp", "opis_menu": "Lamp", "params": 0, "uwagi": "@home"})
	})
	edit(me, "config/group_objects.json", func(r *rows) {
		*r = append(*r, map[string]any{"id": "@member", "id_grupy": 1, "id_obiektu": "@lamp"})
	})
	edit(me, "logic/10a.json", func(f *map[string]any) {
		rules := (*f)["rules"].([]any)
		rules[2].(map[string]any)["link"] = 4
		(*f)["rules"] = append(rules, map[string]any{"value": 12})
	})
	edit(me, "nodered/lights.json", func(r *rows) {
		(*r)[1]["name"] = "every minute"
		*r = append(*r, map[string]any{"id": "n2", "type": "debug", "z": "t1"})
	})

	diff := me.must("diff")
	for _, want := range []string{"+ config/objects/@lamp", `opis_menu: "Hall light" → "Hall lights"`, "rules[2].link: 3 → 4", "rules[3]: added", "+ nodered/n2"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}

	me.must("push")
	if check.must("pull"); strings.Contains(check.read("config/objects.json"), "Lamp") {
		t.Fatal("push without --yes wrote")
	}

	me.must("push", "--yes")
	if out := me.must("diff"); out != "no edits\n" {
		t.Fatalf("edits left after push:\n%s", out)
	}
	check.must("pull")
	objects, members := check.read("config/objects.json"), check.read("config/group_objects.json")
	// @lamp comes after the Fan, numbered 3 by hand; "@home" is a label, not a reference.
	if !strings.Contains(objects, `"id": 4,`) || !strings.Contains(members, `"id_obiektu": 4`) || !strings.Contains(objects, `"uwagi": "@home"`) {
		t.Errorf("placeholders not resolved:\n%s\n%s", objects, members)
	}
	if strings.Contains(objects, "modifiedOrNew") {
		t.Error("modifiedOrNew reached the stored rows")
	}
	if logic := check.read("logic/10a.json"); !strings.Contains(logic, `"value": 12`) || !strings.Contains(logic, `"link": 4`) {
		t.Errorf("rules not written:\n%s", logic)
	}
	if flows := check.read("nodered/lights.json"); !strings.Contains(flows, "every minute") || !strings.Contains(flows, `"n2"`) {
		t.Errorf("flows not deployed:\n%s", flows)
	}

	// Restoring the backup undoes the push, except that the new objects cannot
	// be deleted (the M-SERV only soft-deletes objects).
	backups := strings.Fields(me.must("backups"))
	me.must("restore", backups[0])
	if _, err := me.run("push", "--yes"); err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("restore deleted an object: %v", err)
	}
	me.must("push", "--yes", "--only", "logic,nodered")
	check.must("pull")
	if strings.Contains(check.read("logic/10a.json"), `"value": 12`) || strings.Contains(check.read("nodered/lights.json"), `"n2"`) {
		t.Error("restore did not undo the logic and flow edits")
	}
	if _, err := me.run("restore", ".."); err == nil {
		t.Error("restore accepted a path outside the backups")
	}
}

// A push that fails part-way leaves the rows it wrote with their ids in the
// working copy, so pushing again finishes the job without writing them twice.
func TestPushRetry(t *testing.T) {
	s := house(t)
	me, check := newCopy(t), newCopy(t)
	me.must("pull")
	edit(me, "config/objects.json", func(r *rows) {
		*r = append(*r, map[string]any{"id": "@lamp", "opis_menu": "Lamp", "params": 0})
	})
	edit(me, "config/group_objects.json", func(r *rows) {
		*r = append(*r, map[string]any{"id": "@member", "id_grupy": 1, "id_obiektu": "@lamp"})
	})

	s.Refuse("objects") // saved after group_objects
	if _, err := me.run("push", "--yes"); err == nil || !strings.Contains(err.Error(), "refused the objects save") {
		t.Fatalf("want the objects save refused, got %v", err)
	}
	if out := me.must("push", "--yes"); !strings.Contains(out, "config: 1 already on the M-SERV") {
		t.Errorf("retry did not skip the written member:\n%s", out)
	}
	check.must("pull")
	if n := strings.Count(check.read("config/group_objects.json"), `"id_obiektu": 3`); n != 1 {
		t.Errorf("the new member is stored %d times:\n%s", n, check.read("config/group_objects.json"))
	}
	if n := strings.Count(check.read("config/objects.json"), "Lamp"); n != 1 {
		t.Errorf("the new object is stored %d times", n)
	}
}

func TestRemoveFlowTab(t *testing.T) {
	house(t)
	me, check := newCopy(t), newCopy(t)
	me.must("pull")
	if err := os.Remove(filepath.Join(me.dir, "nodered/lights.json")); err != nil {
		t.Fatal(err)
	}
	if out := me.must("diff"); !strings.Contains(out, "- nodered/t1") || !strings.Contains(out, "- nodered/n1") {
		t.Fatalf("diff:\n%s", out)
	}
	me.must("push", "--yes")
	check.must("pull")
	if _, err := os.Stat(filepath.Join(check.dir, "nodered/lights.json")); err == nil {
		t.Error("the tab is still deployed")
	}
	if !strings.Contains(check.read("nodered/global.json"), `"c1"`) {
		t.Error("removing a tab removed the global nodes")
	}
}

func TestPushRefuses(t *testing.T) {
	house(t)
	me, them := newCopy(t), newCopy(t)
	me.must("pull")
	them.must("pull")

	refuse := func(want string) {
		t.Helper()
		out, err := me.run("push", "--yes")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want an error with %q, got %v\n%s", want, err, out)
		}
		me.must("pull", "--force")
	}

	edit(me, "logic/10a.json", func(f *map[string]any) {
		(*f)["rules"].([]any)[0].(map[string]any)["colour"] = "red"
	})
	refuse("do not survive encoding")

	edit(me, "logic/10a.json", func(f *map[string]any) {
		(*f)["rules"].([]any)[0].(map[string]any)["value"] = 250
	})
	refuse("value out of range")

	edit(me, "config/objects.json", func(r *rows) { *r = (*r)[1:] })
	refuse("cannot be deleted")

	edit(me, "devices.json", func(r *rows) { (*r)[0]["protocol"] = 24 })
	if out := me.must("push", "--yes"); out != "nothing to push\n" {
		t.Fatalf("devices.json is reference only, but push did:\n%s", out)
	}

	edit(them, "config/objects.json", func(r *rows) { (*r)[1]["opis_menu"] = "Their porch" })
	them.must("push", "--yes")
	edit(me, "config/objects.json", func(r *rows) { (*r)[1]["opis_menu"] = "My porch" })
	refuse("changed on the M-SERV since your pull: config/objects/2")

	edit(me, "config/objects.json", func(r *rows) { (*r)[0]["opis_menu"] = "Hall" })
	if _, err := me.run("pull"); err == nil || !strings.Contains(err.Error(), "unpushed edits in config") {
		t.Fatalf("pull over unpushed edits: %v", err)
	}
}
