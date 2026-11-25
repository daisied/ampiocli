// Package logic is the module-logic domain: the rules ("conditions") stored
// in each CAN module, which the modules run on their own, without the M-SERV.
//
//	devices.json      the module list, as the M-SERV reports it (for reference;
//	                  edits to it are ignored)
//	logic/<mac>.json  one module's rules, decoded: {"name", "protocol", "rules"}
//
// A module keeps its rules as bytes in a format only Designer documents, in
// code: a table of conditions, and a table of linked conditions that rules
// with several inputs point into. ampiocli uses Designer's own codec
// (package designer) to decode and encode them, and refuses to write unless
// the codec passes two checks for that module: it reproduces the module's
// current bytes exactly from their decoded rules, and the new bytes decode
// back to exactly the edited rules.
//
// Writes go over MQTT, as Designer sends them:
//
//	publish base64(bytes) to device_api/to/<mac>/conditions_wr
//	progress {"p": percent, "s": status} on device_api/from/<MAC>/status/conditions_wr/0
//	the same for linked_conditions_wr when the linked table is not empty
//
// The answer has the mac in upper case and the bus group (0) at the end.
package logic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/daisied/ampiocli/internal/designer"
	"github.com/daisied/ampiocli/internal/engine"
	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/workdir"
)

// Modules from protocol 25 on store logic in another format, which Designer
// writes through a different API. ampiocli leaves them out.
const newFormatProtocol = 25

type Domain struct {
	conn     *mserv.Conn
	designer string            // Designer's address, where the codec comes from
	codec    *designer.Codec   // loaded on first pull
	devices  map[string]device // by mac, as last pulled
	warn     io.Writer         // modules left out of a pull are reported here
}

func New(conn *mserv.Conn, designerURL string, warn io.Writer) *Domain {
	return &Domain{conn: conn, designer: designerURL, warn: warn}
}

func (*Domain) Name() string { return "logic" }

func (*Domain) Owns(path string) bool {
	return path == "devices.json" || strings.HasPrefix(path, "logic/")
}

// device is the part of a module-list row ampiocli uses.
type device struct {
	MacProd           int64
	Name              []byte // base64 in JSON
	Protocol          int
	Conditions        []byte
	LinkedConditions  []byte
	ConditionsMemSize int
}

func (dev device) mac() string { return strconv.FormatInt(dev.MacProd, 16) }

// macUniq is Designer's id for a module on the CAN bus itself (bus group 0).
func (dev device) macUniq() string { return "0_" + dev.mac() }

// file is logic/<mac>.json.
type file struct {
	Name     string          `json:"name"`
	Protocol int             `json:"protocol"`
	Rules    json.RawMessage `json:"rules"`
}

func (d *Domain) Pull(ctx context.Context) (workdir.Files, error) {
	raw, devices, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	if d.codec == nil {
		if d.codec, err = designer.Open(d.designer, raw); err != nil {
			return nil, err
		}
	}
	files := workdir.Files{"devices.json": raw}
	d.devices = map[string]device{}
	for _, dev := range devices {
		d.devices[dev.mac()] = dev
		if dev.Protocol >= newFormatProtocol {
			continue
		}
		rules := json.RawMessage("[]")
		if len(dev.Conditions) > 0 {
			if rules, err = d.codec.Decode(dev.macUniq(), dev.Conditions, dev.LinkedConditions); err != nil {
				// One module the codec cannot read does not stop the others.
				fmt.Fprintf(d.warn, "logic: module %s left out, Designer %s cannot decode it: %v\n", dev.mac(), d.codec.Version, err)
				continue
			}
		}
		if files["logic/"+dev.mac()+".json"], err = json.Marshal(file{string(dev.Name), dev.Protocol, rules}); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// list reads the module list: as the M-SERV sends it, and decoded.
func (d *Domain) list(ctx context.Context) (json.RawMessage, []device, error) {
	raw, err := d.conn.Get(ctx, "device_api/to/list", "", "device_api/from/list")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the module list: %w", err)
	}
	var list struct{ Devices json.RawMessage }
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, fmt.Errorf("reading the module list: %w", err)
	}
	var devices []device
	if err := json.Unmarshal(list.Devices, &devices); err != nil {
		return nil, nil, fmt.Errorf("reading the module list: %w", err)
	}
	return list.Devices, devices, nil
}

func (*Domain) Items(files workdir.Files) (items.Items, error) {
	out := items.Items{}
	for path, raw := range files {
		if path == "devices.json" {
			continue
		}
		v, err := items.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, fmt.Errorf(`%s: expected {"name", "protocol", "rules"}`, path)
		}
		out[strings.TrimSuffix(path, ".json")] = v
	}
	return out, nil
}

// write is one module's new logic.
type write struct {
	dev                device
	conditions, linked []byte
}

// Prepare works on the module list of the pull the engine ran just before.
func (d *Domain) Prepare(changes []items.Change, _ items.Items, _ workdir.Files) (*engine.Plan, error) {
	var writes []write
	var notes []string
	for _, c := range changes {
		if c.Added || c.Removed {
			return nil, fmt.Errorf("%s: modules cannot be added or removed; only their rules can change", c.Key)
		}
		before, after := c.Before.(map[string]any), c.After.(map[string]any)
		if !items.Equal(before["name"], after["name"]) || !items.Equal(before["protocol"], after["protocol"]) {
			return nil, fmt.Errorf("%s: only rules can change (rename modules in Designer)", c.Key)
		}
		// The conflict check has made sure the module is still there, and new-format
		// modules have no logic file to change.
		w, note, err := d.encode(d.devices[strings.TrimPrefix(c.Key, "logic/")], after["rules"])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Key, err)
		}
		if note != "" {
			notes = append(notes, c.Key+": "+note)
		}
		writes = append(writes, w)
	}
	return &engine.Plan{
		Notes:  notes,
		Expect: changes,
		Write: func(ctx context.Context) error {
			for _, w := range writes {
				if err := d.send(ctx, w.dev, "conditions_wr", w.conditions); err != nil {
					return err
				}
				// Designer only sends the linked table when there is one.
				if len(w.linked) > 0 {
					if err := d.send(ctx, w.dev, "linked_conditions_wr", w.linked); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}, nil
}

// encode runs the codec checks and returns the bytes to write, and a note
// when the write will differ from the module beyond the edit.
func (d *Domain) encode(dev device, rules any) (w write, note string, err error) {
	mac := dev.macUniq()
	current, err := d.codec.Decode(mac, dev.Conditions, dev.LinkedConditions)
	if err != nil {
		return write{}, "", err
	}
	// Self-test: the module's own rules must encode back to its bytes.
	again, againLinked, err := d.codec.Encode(mac, current)
	if err != nil {
		return write{}, "", err
	}
	if !sameTable(dev.Conditions, again) {
		return write{}, "", fmt.Errorf("Designer %s's codec does not reproduce this module's current bytes; not writing it", d.codec.Version)
	}
	if !sameTable(dev.LinkedConditions, againLinked) {
		// The encoder rebuilds the linked table from the rules, so entries no
		// rule points to drop out. That is all it may change: the module's
		// conditions must read the same with the rebuilt table.
		same, err := d.sameRules(mac, dev.Conditions, againLinked, current)
		if err != nil || !same {
			return write{}, "", fmt.Errorf("Designer %s's codec does not reproduce this module's linked table; not writing it", d.codec.Version)
		}
		note = "the module's linked table holds entries no rule uses; Designer leaves them too"
	}

	// Round trip: the new bytes must decode to exactly the edited rules.
	edited, err := json.Marshal(rules)
	if err != nil {
		return write{}, "", err
	}
	conditions, linked, err := d.codec.Encode(mac, edited)
	if err != nil {
		return write{}, "", fmt.Errorf("encoding: %w", err)
	}
	if len(conditions) == 0 || conditions[len(conditions)-1] != 255 {
		return write{}, "", errors.New("the encoder returned bytes without the end mark")
	}
	if len(conditions) > dev.ConditionsMemSize {
		return write{}, "", fmt.Errorf("the rules need %d bytes; the module has room for %d", len(conditions), dev.ConditionsMemSize)
	}
	back, err := d.codec.Decode(mac, conditions[:len(conditions)-1], linked)
	if err != nil {
		return write{}, "", fmt.Errorf("decoding the new bytes: %w", err)
	}
	got, err := items.Decode(back)
	if err != nil {
		return write{}, "", err
	}
	if lost := items.Fields(rules, got); len(lost) > 0 {
		return write{}, "", fmt.Errorf("the edited rules do not survive encoding (unknown field or value out of range; copy an existing rule and change it):\n  rules%s",
			strings.Join(lost, "\n  rules"))
	}
	return write{dev, conditions, linked}, note, nil
}

// sameRules reports whether conditions decode to rules with the linked table linked.
func (d *Domain) sameRules(mac string, conditions, linked []byte, rules json.RawMessage) (bool, error) {
	got, err := d.codec.Decode(mac, conditions, linked)
	if err != nil {
		return false, err
	}
	a, err := items.Decode(got)
	if err != nil {
		return false, err
	}
	b, err := items.Decode(rules)
	return err == nil && items.Equal(a, b), err
}

// send writes one table to a module and waits for it to confirm.
func (d *Domain) send(ctx context.Context, dev device, command string, data []byte) error {
	topic := fmt.Sprintf("device_api/to/%s/%s", dev.mac(), command)
	status := fmt.Sprintf("device_api/from/%s/status/%s/0", strings.ToUpper(dev.mac()), command)
	payload := []byte(base64.StdEncoding.EncodeToString(data))
	var last []byte
	err := d.conn.Call(ctx, topic, payload, status, time.Minute, func(reply []byte) (bool, error) {
		last = reply
		var s struct{ P, S int }
		if err := json.Unmarshal(reply, &s); err != nil {
			return false, fmt.Errorf("module %s: unreadable status %q", dev.mac(), reply)
		}
		if s.S < 0 {
			return false, fmt.Errorf("module %s refused %s (status %d)", dev.mac(), command, s.S)
		}
		return s.P >= 100 && s.S == 0, nil
	})
	if errors.Is(err, mserv.ErrNoAnswer) && last != nil {
		return fmt.Errorf("module %s did not finish %s; its last status was %s", dev.mac(), command, last)
	}
	return err
}

// sameTable says whether a table in the module list holds encoded, the
// encoder's output, which ends with a 255 end mark. The list shows a table
// as last written, end mark included, or as read from the module, without it.
// The codec reads both alike.
func sameTable(listed, encoded []byte) bool {
	return bytes.Equal(listed, encoded) || bytes.Equal(listed, bytes.TrimSuffix(encoded, []byte{255}))
}
