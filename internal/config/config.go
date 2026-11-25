// Package config is the app configuration domain: the M-SERV database tables
// behind the Ampio app (objects, groups, locations, ...). One file per table,
// config/<table>.json, holding its rows; an item per row, config/<table>/<id>.
//
// Tables are read and saved over MQTT exactly as Designer does it:
//
//	read:  publish "<table>" to ampio/control/admin/config
//	       reply  {"List":[rows]} on ampio/fromDB/admin/config/<table>
//	save:  publish {"List":[changed rows]} to ampio/control/admin/config/save/<table>,
//	       or save/<table>_nc for the tables rows can be removed from, which
//	       also take {"idToRemove": id} entries
//	       reply  "0" on ampio/fromDB/admin/config/statusSave/<code>
//	then:  publish "refresh" to ampio/control/admin/data (the app reloads)
//
// New rows can use a placeholder id such as "@hall_light"; it becomes the
// table's next free id, and any column of another new or changed row that
// refers to a row (id_obiektu, id_grupy, lokalizacja, ...) and holds the same
// placeholder gets that id too: a group member pointing at the new object, say.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/daisied/ampiocli/internal/engine"
	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/workdir"
)

type table struct {
	name string
	// code is the number in the save-status topic; 0 means ampiocli only reads the table.
	code int
	// removable tables are saved on the "<table>_nc" topic, which also takes
	// {"idToRemove": id}. The others cannot delete rows: Designer hides a
	// deleted object or group by setting the DELETED bit (16) in its params.
	removable bool
}

// tables in Designer's save order. users, users_tokens and roles hold logins
// and are left out on purpose.
var tables = []table{
	{"groups", 2, false},
	{"group_objects", 7, true},
	{"resources", 4, false},
	{"connected_objects", 1, true},
	{"locations", 8, true},
	{"objects", 6, false},
	{"flag_conditions", 10, false},
	{"data", 9, false},
	{"conditions", 0, false},
	{"scenes", 0, false},
}

var (
	placeholder = regexp.MustCompile(`^@[A-Za-z0-9_-]+$`)
	// reference matches the columns that hold a row id.
	reference = regexp.MustCompile(`^(id|id_.+|lokalizacja)$`)
)

type Domain struct{ conn *mserv.Conn }

func New(conn *mserv.Conn) *Domain { return &Domain{conn} }

func (*Domain) Name() string { return "config" }

func (*Domain) Owns(path string) bool { return strings.HasPrefix(path, "config/") }

func (d *Domain) Pull(ctx context.Context) (workdir.Files, error) {
	files := workdir.Files{}
	for _, t := range tables {
		raw, err := d.conn.Get(ctx, "ampio/control/admin/config", t.name, "ampio/fromDB/admin/config/"+t.name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", t.name, err)
		}
		var reply struct{ List json.RawMessage }
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, fmt.Errorf("reading %s: %w", t.name, err)
		}
		if string(reply.List) == "null" || len(reply.List) == 0 {
			reply.List = json.RawMessage("[]")
		}
		files["config/"+t.name+".json"] = reply.List
	}
	return files, nil
}

func (*Domain) Items(files workdir.Files) (items.Items, error) {
	out := items.Items{}
	for path, raw := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "config/"), ".json")
		if _, ok := lookup(name); !ok {
			return nil, fmt.Errorf("%s: not a table ampiocli knows", path)
		}
		v, err := items.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rows, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a list of rows", path)
		}
		for i, r := range rows {
			row, ok := r.(map[string]any)
			if !ok || row["id"] == nil {
				return nil, fmt.Errorf("%s: row %d has no id", path, i)
			}
			k := key(name, row["id"])
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%s: two rows with id %v", path, row["id"])
			}
			out[k] = row
		}
	}
	return out, nil
}

func lookup(name string) (table, bool) {
	for _, t := range tables {
		if t.name == name {
			return t, true
		}
	}
	return table{}, false
}

// key is the item key of a table row.
func key(table string, id any) string { return fmt.Sprintf("config/%s/%v", table, id) }

// split turns a row's key into its parts.
func split(key string) (table, string) {
	name, id, _ := strings.Cut(strings.TrimPrefix(key, "config/"), "/")
	t, _ := lookup(name)
	return t, id
}

func (d *Domain) Prepare(changes []items.Change, live items.Items, work workdir.Files) (*engine.Plan, error) {
	ids, err := assignIDs(work, live)
	if err != nil {
		return nil, err
	}
	saves := map[string][]any{} // table → rows and removals to send
	plan := &engine.Plan{}
	for _, c := range changes {
		t, _ := split(c.Key)
		if t.code == 0 {
			return nil, fmt.Errorf("%s: ampiocli does not write table %s", c.Key, t.name)
		}
		if c.Removed {
			if !t.removable {
				return nil, fmt.Errorf("%s: rows in %s cannot be deleted; set the DELETED bit (16) in params instead, as Designer does", c.Key, t.name)
			}
			saves[t.name] = append(saves[t.name], map[string]any{"idToRemove": c.Before.(map[string]any)["id"]})
			plan.Expect = append(plan.Expect, c)
			continue
		}
		row := maps.Clone(c.After.(map[string]any))
		if _, err := resolve(row, ids); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Key, err)
		}
		id, _ := row["id"].(json.Number)
		if _, err := strconv.Atoi(string(id)); err != nil {
			return nil, fmt.Errorf("%s: a row's id must be a whole number, or for a new row a placeholder like \"@hall_light\"", c.Key)
		}
		plan.Expect = append(plan.Expect, items.Change{Key: key(t.name, id), After: maps.Clone(row)})
		if c.Added {
			row["modifiedOrNew"] = 1 // Designer's marks for new and changed rows
		} else {
			row["modifiedOrNew"] = true
		}
		saves[t.name] = append(saves[t.name], row)
	}

	if len(ids) > 0 {
		if plan.Work, err = resolveFiles(work, ids); err != nil {
			return nil, err
		}
	}
	plan.Write = func(ctx context.Context) error {
		for _, t := range tables {
			if rows := saves[t.name]; rows != nil {
				if err := d.save(ctx, t, rows); err != nil {
					return err
				}
			}
		}
		return nil
	}
	plan.After = func(context.Context) error { return d.conn.Publish("ampio/control/admin/data", "refresh") }
	return plan, nil
}

// assignIDs numbers the new rows that have a placeholder id in the order
// they appear in their file, after the table's live rows and the new rows
// numbered by hand.
func assignIDs(work workdir.Files, live items.Items) (map[string]json.Number, error) {
	highest := map[string]int{}
	for key := range live {
		t, id := split(key)
		if n, err := strconv.Atoi(id); err == nil {
			highest[t.name] = max(highest[t.name], n)
		}
	}
	ids := map[string]json.Number{}
	for _, t := range tables {
		raw, ok := work["config/"+t.name+".json"]
		if !ok {
			continue
		}
		v, err := items.Decode(raw)
		if err != nil {
			return nil, err
		}
		rows, _ := v.([]any)
		var news []string
		for _, r := range rows {
			switch id := r.(map[string]any)["id"].(type) {
			case json.Number:
				if n, err := strconv.Atoi(string(id)); err == nil {
					highest[t.name] = max(highest[t.name], n)
				}
			case string:
				if placeholder.MatchString(id) {
					news = append(news, id)
				}
			}
		}
		for _, id := range news {
			if _, dup := ids[id]; dup {
				return nil, fmt.Errorf("%s is the id of two new rows; give each its own placeholder", id)
			}
			highest[t.name]++
			ids[id] = json.Number(strconv.Itoa(highest[t.name]))
		}
	}
	return ids, nil
}

// resolve replaces the placeholders in a row's id columns with their ids,
// and reports whether there were any.
func resolve(row map[string]any, ids map[string]json.Number) (bool, error) {
	found := false
	for field, v := range row {
		s, ok := v.(string)
		if !ok || !reference.MatchString(field) || !placeholder.MatchString(s) {
			continue
		}
		n, known := ids[s]
		if !known {
			return false, fmt.Errorf("%s is %q, but no new row has that id", field, s)
		}
		row[field], found = n, true
	}
	return found, nil
}

// resolveFiles returns the working copy with its placeholders resolved.
func resolveFiles(work workdir.Files, ids map[string]json.Number) (workdir.Files, error) {
	out := workdir.Files{}
	for path, raw := range work {
		out[path] = raw
		v, err := items.Decode(raw)
		if err != nil {
			return nil, err
		}
		rows, _ := v.([]any)
		changed := false
		for _, r := range rows {
			found, err := resolve(r.(map[string]any), ids)
			if err != nil {
				return nil, err
			}
			changed = changed || found
		}
		if changed {
			if out[path], err = json.Marshal(rows); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (d *Domain) save(ctx context.Context, t table, rows []any) error {
	topic := "ampio/control/admin/config/save/" + t.name
	if t.removable {
		topic += "_nc"
	}
	payload, err := json.Marshal(map[string]any{"List": rows})
	if err != nil {
		return err
	}
	status := fmt.Sprintf("ampio/fromDB/admin/config/statusSave/%d", t.code)
	return d.conn.Call(ctx, topic, payload, status, 30*time.Second, func(reply []byte) (bool, error) {
		if string(reply) != "0" {
			return false, fmt.Errorf("the M-SERV refused the %s save (status %s)", t.name, reply)
		}
		return true, nil
	})
}
