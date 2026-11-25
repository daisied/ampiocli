package logic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/daisied/ampiocli/internal/designer"
	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/workdir"
)

// TestRealModules runs every module of a real installation through the real
// Designer codec: decode, then both write checks on the rules unchanged. It
// reads only local files, and runs only when given them:
//
//	AMPIOCLI_REAL           a working copy after a pull (its devices.json)
//	AMPIOCLI_REAL_DESIGNER  that M-SERV's Designer cache, filled by the pull
//	                        (~/.cache/ampiocli/designer/<host>)
func TestRealModules(t *testing.T) {
	copyDir, cache := os.Getenv("AMPIOCLI_REAL"), os.Getenv("AMPIOCLI_REAL_DESIGNER")
	if copyDir == "" || cache == "" {
		t.Skip("set AMPIOCLI_REAL and AMPIOCLI_REAL_DESIGNER to run against a real installation")
	}
	files, err := workdir.Copy(copyDir).Work().Read(func(p string) bool { return p == "devices.json" })
	if err != nil || files["devices.json"] == nil {
		t.Fatalf("no devices.json in %s: %v", copyDir, err)
	}
	raw := files["devices.json"]
	var devices []device
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // the served copy gets a cache of its own
	web := httptest.NewServer(http.FileServer(http.Dir(cache)))
	t.Cleanup(web.Close)
	c, err := designer.Open(web.URL, raw)
	if err != nil {
		t.Fatal(err)
	}
	d := &Domain{codec: c}

	tested := 0
	for _, dev := range devices {
		if dev.Protocol >= newFormatProtocol || len(dev.Conditions) == 0 {
			continue
		}
		rules, err := c.Decode(dev.macUniq(), dev.Conditions, dev.LinkedConditions)
		if err != nil {
			t.Errorf("module %s: decode: %v", dev.mac(), err)
			continue
		}
		v, err := items.Decode(rules)
		if err != nil {
			t.Fatal(err)
		}
		w, note, err := d.encode(dev, v)
		if err != nil {
			t.Errorf("module %s: %v", dev.mac(), err)
			continue
		}
		if !sameTable(dev.Conditions, w.conditions) {
			t.Errorf("module %s: unchanged rules encode to other bytes", dev.mac())
		}
		t.Logf("module %s: %d rules, %d + %d bytes, codec checks pass%s", dev.mac(), len(v.([]any)), len(w.conditions), len(w.linked), strings.TrimPrefix(" ("+note+")", " ()"))
		tested++
	}
	if tested == 0 {
		t.Fatal("no module with logic")
	}
}
