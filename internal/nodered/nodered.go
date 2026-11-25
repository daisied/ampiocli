// Package nodered is the Node-RED domain: the flows of the Node-RED that
// runs on the M-SERV, through Node-RED's documented admin HTTP API.
//
//	nodered/<tab>.json   one tab or subflow: its own node first, then the nodes on it
//	nodered/global.json  configuration nodes that belong to no tab
//
// An item is one node, nodered/<node id>. A push sends the live flows with
// the edited nodes replaced, removed or added. It names the revision of the
// flows it read just before, so Node-RED refuses it (409) if someone
// deployed in the moment between; edits made since the pull are caught
// earlier, by the engine's conflict check. Node credentials are never read;
// nodes sent without them keep the ones they have.
package nodered

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/daisied/ampiocli/internal/engine"
	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/workdir"
)

type Domain struct {
	settings mserv.Settings
	http     *http.Client
	token    string // admin API token, once logged in

	// The flows as last read. Node-RED keeps nodes in this order, and rev
	// is the revision a deploy must name.
	rev   string
	flows []json.RawMessage
}

func New(s mserv.Settings) *Domain {
	return &Domain{settings: s, http: &http.Client{Timeout: 30 * time.Second}}
}

func (*Domain) Name() string { return "nodered" }

func (*Domain) Owns(path string) bool { return strings.HasPrefix(path, "nodered/") }

// node is the part of a node that places it in a file.
type node struct {
	ID, Type, Z, Label, Name string
}

func (d *Domain) Pull(ctx context.Context) (workdir.Files, error) {
	var state struct {
		Rev   string
		Flows []json.RawMessage
	}
	if err := d.call(ctx, http.MethodGet, nil, &state); err != nil {
		return nil, err
	}
	d.rev, d.flows = state.Rev, state.Flows

	nodes := make([]node, len(state.Flows))
	file := map[string]string{} // tab or subflow id → its file
	taken := map[string]bool{"global": true}
	for i, raw := range state.Flows {
		if err := json.Unmarshal(raw, &nodes[i]); err != nil {
			return nil, err
		}
		if n := nodes[i]; n.Type == "tab" || n.Type == "subflow" {
			name := slug(cmp.Or(n.Label, n.Name))
			if name == "" || taken[name] {
				name = strings.Trim(name+"-"+n.ID, "-")
			}
			taken[name] = true
			file[n.ID] = "nodered/" + name + ".json"
		}
	}
	grouped := map[string][]json.RawMessage{}
	for i, n := range nodes {
		path, ok := file[n.ID]
		if !ok {
			path, ok = file[n.Z]
		}
		if !ok {
			path = "nodered/global.json"
		}
		grouped[path] = append(grouped[path], state.Flows[i])
	}
	files := workdir.Files{}
	for path, list := range grouped {
		raw, err := json.Marshal(list)
		if err != nil {
			return nil, err
		}
		files[path] = raw
	}
	return files, nil
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func (*Domain) Items(files workdir.Files) (items.Items, error) {
	out := items.Items{}
	for path, raw := range files {
		v, err := items.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a list of nodes", path)
		}
		for i, n := range list {
			m, _ := n.(map[string]any)
			id, _ := m["id"].(string)
			if id == "" {
				return nil, fmt.Errorf("%s: node %d has no id", path, i)
			}
			if _, dup := out["nodered/"+id]; dup {
				return nil, fmt.Errorf("%s: node id %s is used twice", path, id)
			}
			out["nodered/"+id] = m
		}
	}
	return out, nil
}

func (d *Domain) Prepare(changes []items.Change, _ items.Items, _ workdir.Files) (*engine.Plan, error) {
	edited := map[string]items.Change{}
	for _, c := range changes {
		edited[strings.TrimPrefix(c.Key, "nodered/")] = c
	}
	var flows []any
	for _, raw := range d.flows {
		var n node
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		c, ok := edited[n.ID]
		switch {
		case !ok:
			flows = append(flows, raw)
		case !c.Removed:
			flows = append(flows, c.After)
		}
	}
	for _, c := range changes {
		if c.Added {
			flows = append(flows, c.After)
		}
	}
	body, err := json.Marshal(map[string]any{"rev": d.rev, "flows": flows})
	if err != nil {
		return nil, err
	}
	return &engine.Plan{
		Expect: changes,
		Write:  func(ctx context.Context) error { return d.call(ctx, http.MethodPost, body, nil) },
	}, nil
}

// call sends one request to /flows, logging in first if Node-RED asks for it.
func (d *Domain) call(ctx context.Context, method string, body []byte, out any) error {
	if err := d.login(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, d.settings.NodeRED+"/flows", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Node-RED-API-Version", "v2")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Node-RED-Deployment-Type", "flows") // restart only the flows that changed
	}
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	return d.do(req, out)
}

// login gets an admin API token when Node-RED's admin auth is on.
func (d *Domain) login(ctx context.Context) error {
	if d.token != "" {
		return nil
	}
	if err := mserv.Need(d.settings.NodeRED, d.settings.NodeREDPassword); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.settings.NodeRED+"/auth/login", nil)
	if err != nil {
		return err
	}
	var scheme struct{ Type string }
	if err := d.do(req, &scheme); err != nil {
		return err
	}
	if scheme.Type != "credentials" {
		return nil // admin auth is off
	}
	form := url.Values{
		"client_id":  {"node-red-admin"},
		"grant_type": {"password"},
		"scope":      {"*"},
		"username":   {d.settings.NodeREDUser},
		"password":   {d.settings.NodeREDPassword},
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, d.settings.NodeRED+"/auth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := d.do(req, &token); err != nil {
		return fmt.Errorf("%w (check NODERED_USER / NODERED_PASSWORD)", err)
	}
	d.token = token.AccessToken
	return nil
}

// do sends req and decodes the JSON reply into out, if out is set.
func (d *Domain) do(req *http.Request, out any) error {
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("Node-RED: %w", err)
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("someone deployed Node-RED flows while ampiocli was pushing; nothing written to Node-RED, pull and try again")
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent:
		return fmt.Errorf("Node-RED %s %s: %s %s", req.Method, req.URL.Path, resp.Status, bytes.TrimSpace(reply))
	case out != nil:
		return json.Unmarshal(reply, out)
	}
	return nil
}
