// Package sim is a stand-in M-SERV: an MQTT broker and a web server that
// answer everything ampiocli sends the way the real M-SERV does, keeping the
// state in memory. It starts from a working copy's files, so pushes can be
// tried on a copy of a real installation without touching it.
//
// Designer's files are served from a folder, normally ampiocli's cache of the
// real Designer (filled by a real pull).
package sim

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/workdir"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// saveCodes number the save-status topics, as the M-SERV does. Package
// config has the same numbers; the simulator keeps its own copy, so that it
// stays an independent model of the M-SERV rather than a mirror of ampiocli.
var saveCodes = map[string]int{
	"connected_objects": 1, "groups": 2, "resources": 4, "objects": 6,
	"group_objects": 7, "locations": 8, "data": 9, "flag_conditions": 10,
}

type Sim struct {
	Settings mserv.Settings // how to reach the simulator

	mu      sync.Mutex
	tables  map[string][]any
	devices []any
	flows   []any
	rev     int
	refuse  map[string]bool // tables whose next save fails
	log     io.Writer

	broker *mqtt.Server
	web    *http.Server
}

// Start serves files (a working copy's tree), and Designer from designerDir,
// on local ports. Every write is reported on log.
func Start(files workdir.Files, designerDir string, log io.Writer) (*Sim, error) {
	s := &Sim{tables: map[string][]any{}, refuse: map[string]bool{}, log: log}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		v, err := items.Decode(files[p])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		list, _ := v.([]any)
		switch {
		case p == "devices.json":
			s.devices = list
		case strings.HasPrefix(p, "config/"):
			s.tables[strings.TrimSuffix(path.Base(p), ".json")] = list
		case strings.HasPrefix(p, "nodered/"):
			s.flows = append(s.flows, list...)
		}
	}

	s.broker = mqtt.New(&mqtt.Options{InlineClient: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	tcp := listeners.NewTCP(listeners.Config{ID: "sim", Address: "127.0.0.1:0"})
	if err := s.broker.AddHook(new(auth.AllowHook), nil); err != nil {
		return nil, err
	}
	if err := s.broker.AddListener(tcp); err != nil {
		return nil, err
	}
	for filter, handle := range map[string]func(topic string, payload []byte){
		"ampio/control/admin/config":        s.readTable,
		"ampio/control/admin/config/save/+": s.saveTable,
		"device_api/to/list":                s.listDevices,
		"device_api/to/+/+":                 s.writeModule,
	} {
		err := s.broker.Subscribe(filter, 1, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
			handle(pk.TopicName, pk.Payload)
		})
		if err != nil {
			return nil, err
		}
	}
	if err := s.broker.Serve(); err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.broker.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("{}")) })
	mux.HandleFunc("GET /flows", s.getFlows)
	mux.HandleFunc("POST /flows", s.postFlows)
	mux.Handle("GET /", http.FileServer(http.Dir(designerDir)))
	s.web = &http.Server{Handler: mux}
	go s.web.Serve(ln)

	web := "http://" + ln.Addr().String()
	s.Settings = mserv.Settings{MQTT: "mqtt://" + tcp.Address(), Designer: web, NodeRED: web,
		User: "admin", Password: "sim", NodeREDUser: "admin", NodeREDPassword: "sim"}
	return s, nil
}

func (s *Sim) Close() {
	s.web.Close()
	s.broker.Close()
}

// Refuse makes the next save of table fail, as when the M-SERV rejects one.
func (s *Sim) Refuse(table string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[table] = true
}

func (s *Sim) publish(topic string, payload []byte) { s.broker.Publish(topic, payload, false, 0) }

func (s *Sim) publishJSON(topic string, v any) {
	payload, _ := json.Marshal(v)
	s.publish(topic, payload)
}

func (s *Sim) readTable(_ string, payload []byte) {
	s.mu.Lock()
	rows := s.tables[string(payload)]
	s.mu.Unlock()
	if rows == nil {
		rows = []any{}
	}
	s.publishJSON("ampio/fromDB/admin/config/"+string(payload), map[string]any{"List": rows})
}

func (s *Sim) saveTable(topic string, payload []byte) {
	name := strings.TrimSuffix(path.Base(topic), "_nc")
	v, err := items.Decode(payload)
	if err != nil {
		return // the M-SERV ignores what it cannot read
	}
	save, _ := v.(map[string]any)
	list, _ := save["List"].([]any)
	status := fmt.Sprintf("ampio/fromDB/admin/config/statusSave/%d", saveCodes[name])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse[name] {
		delete(s.refuse, name)
		fmt.Fprintf(s.log, "config %s: refused the save\n", name)
		s.publish(status, []byte("1"))
		return
	}
	for _, e := range list {
		entry, _ := e.(map[string]any)
		if id, ok := entry["idToRemove"]; ok {
			s.tables[name] = slices.DeleteFunc(s.tables[name], func(r any) bool { return sameID(r, id) })
			fmt.Fprintf(s.log, "config %s: removed row %v\n", name, id)
			continue
		}
		delete(entry, "modifiedOrNew")
		i := slices.IndexFunc(s.tables[name], func(r any) bool { return sameID(r, entry["id"]) })
		if i < 0 {
			s.tables[name] = append(s.tables[name], entry)
		} else {
			s.tables[name][i] = entry
		}
		fmt.Fprintf(s.log, "config %s: saved row %v\n", name, entry["id"])
	}
	s.publish(status, []byte("0"))
}

func sameID(row any, id any) bool {
	r, _ := row.(map[string]any)
	return fmt.Sprint(r["id"]) == fmt.Sprint(id)
}

func (s *Sim) listDevices(string, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishJSON("device_api/from/list", map[string]any{"devices": s.devices})
}

// writeModule takes device_api/to/<mac>/<command> with base64 bytes. A module
// stores its rules without the 255 end mark the encoder appends.
func (s *Sim) writeModule(topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	mac, command := parts[2], parts[3]
	field := map[string]string{"conditions_wr": "conditions", "linked_conditions_wr": "linkedConditions"}[command]
	data, err := base64.StdEncoding.DecodeString(string(payload))
	if field == "" || err != nil {
		return
	}
	s.mu.Lock()
	for _, d := range s.devices {
		row, _ := d.(map[string]any)
		if n, _ := row["macProd"].(json.Number).Int64(); strconv.FormatInt(n, 16) == mac {
			row[field] = base64.StdEncoding.EncodeToString(bytes.TrimSuffix(data, []byte{255}))
			fmt.Fprintf(s.log, "module %s: wrote %s (%d bytes)\n", mac, field, len(data))
		}
	}
	s.mu.Unlock()
	s.publishJSON(fmt.Sprintf("device_api/from/%s/status/%s", mac, command), map[string]int{"p": 100, "s": 0})
}

func (s *Sim) getFlows(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"rev": fmt.Sprint(s.rev), "flows": s.flows})
}

func (s *Sim) postFlows(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	v, err2 := items.Decode(body)
	deploy, _ := v.(map[string]any)
	flows, ok := deploy["flows"].([]any)
	if err != nil || err2 != nil || !ok {
		http.Error(w, "bad deploy", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if deploy["rev"] != fmt.Sprint(s.rev) {
		http.Error(w, `{"code":"version_mismatch"}`, http.StatusConflict)
		return
	}
	s.flows = flows
	s.rev++
	fmt.Fprintf(s.log, "nodered: deployed %d nodes\n", len(s.flows))
	json.NewEncoder(w).Encode(map[string]any{"rev": fmt.Sprint(s.rev)})
}
