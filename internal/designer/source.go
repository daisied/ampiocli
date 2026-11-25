package designer

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// anchor marks the bundle chunk that holds the codec: only Designer's logic
// save function calls this registry method.
const anchor = ".updateConditionsArrByMacUniq("

var (
	entryScript  = regexp.MustCompile(`<script[^>]*type="module"[^>]*src="([^"]+)"`)
	staticImport = regexp.MustCompile(`from\s*"(\./[\w.-]+\.js)"`)
)

// source reads Designer's files from the M-SERV's web server and keeps a copy
// in the user's cache folder.
type source struct {
	base  *url.URL
	cache string
	http  *http.Client
}

// CacheDir is where the files of the Designer at designerURL are kept between runs.
func CacheDir(designerURL string) (string, error) {
	u, err := url.Parse(designerURL)
	if err != nil {
		return "", err
	}
	dir, err := os.UserCacheDir()
	return filepath.Join(dir, "ampiocli", "designer", strings.ReplaceAll(u.Host, ":", "_")), err
}

func newSource(designerURL string) (*source, error) {
	base, err := url.Parse(designerURL)
	if err != nil || base.Hostname() == "" {
		return nil, fmt.Errorf("no M-SERV address in %q: set AMPIO_HOST in the environment or in .env", designerURL)
	}
	cache, err := CacheDir(designerURL)
	if err != nil {
		return nil, err
	}
	return &source{base: base, cache: cache, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

// get returns one file of the Designer site. Asset names carry a content
// hash, so a cached asset never goes stale; the index page names the current
// assets and is always fetched.
func (s *source) get(p string) ([]byte, error) {
	file := filepath.Join(s.cache, filepath.FromSlash(path.Clean("/"+p)))
	if p == "/" {
		file = filepath.Join(s.cache, "index.html")
	} else if b, err := os.ReadFile(file); err == nil {
		return b, nil
	}
	b, err := s.download(p)
	if err != nil {
		return nil, fmt.Errorf("Designer file %s: %w", p, err)
	}
	// Write and rename, so an interrupted download never stays in the cache.
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".download-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp.Name(), file)
}

func (s *source) download(p string) ([]byte, error) {
	resp, err := s.http.Get(s.base.ResolveReference(&url.URL{Path: p}).String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// codecChunk follows index.html to its entry script and that script's static
// imports, and returns the path of the one that contains the codec.
func (s *source) codecChunk() (string, error) {
	index, err := s.get("/")
	if err != nil {
		return "", err
	}
	m := entryScript.FindSubmatch(index)
	if m == nil {
		return "", errors.New("Designer index.html has no module script")
	}
	entry := string(m[1])
	js, err := s.get(entry)
	if err != nil {
		return "", err
	}
	for _, imp := range staticImport.FindAllSubmatch(js, -1) {
		p := resolve(entry, string(imp[1]))
		chunk, err := s.get(p)
		if err != nil {
			return "", err
		}
		if strings.Contains(string(chunk), anchor) {
			return p, nil
		}
	}
	return "", errors.New("Designer changed: no bundle chunk contains the module-logic codec")
}

// resolve turns an import specifier ("./x.js") into a site path, relative to the importing file.
func resolve(from, spec string) string { return path.Join(path.Dir(from), spec) }
