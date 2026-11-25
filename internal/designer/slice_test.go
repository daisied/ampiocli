package designer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testdata/designer is a miniature Designer bundle with the same shapes as
// the real one, plus browser-only code that must be left out.
func miniature(t *testing.T) (*program, *chunk) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	web := httptest.NewServer(http.FileServer(http.Dir("../../testdata/designer")))
	t.Cleanup(web.Close)
	src, err := newSource(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	file, err := src.codecChunk()
	if err != nil {
		t.Fatal(err)
	}
	p := &program{src: src, chunks: map[string]*chunk{}}
	c, err := p.chunk(file)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func TestCodecNames(t *testing.T) {
	_, c := miniature(t)
	names, err := c.codecNames()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"encode": "pE", "decode": "vD", "registry": "y2", "init": "Js", "addDevice": "wD"}
	for k, v := range want {
		if names[k] != v {
			t.Errorf("%s = %q, want %q", k, names[k], v)
		}
	}
}

func TestProgramKeepsOnlyTheCodec(t *testing.T) {
	p, c := miniature(t)
	names, err := c.codecNames()
	if err != nil {
		t.Fatal(err)
	}
	js, err := p.build(c, names)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"document", "useLogic", "boot", "addEventListener"} {
		if strings.Contains(js, gone) {
			t.Errorf("program contains %s:\n%s", gone, js)
		}
	}
	for _, kept := range []string{"var De = importShared;", `"use strict"`, "window.macPrefix", `var {"h": Hx, "r": Rm} = await`, "class Rg", "Rg.prototype.size = function", `var Pre = "0";`, `var Pre = Pre + "_";`} {
		if !strings.Contains(js, kept) {
			t.Errorf("program lacks %s:\n%s", kept, js)
		}
	}
	// names-T5.js is imported by two chunks, and must run once, like an ES module.
	if n := strings.Count(js, `__ampiocli["/assets/names-T5.js"] = `); n != 1 {
		t.Errorf("names-T5.js is defined %d times:\n%s", n, js)
	}
}
