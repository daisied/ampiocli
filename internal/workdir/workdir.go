// Package workdir is the working copy: a folder of JSON files describing the
// house, like a git checkout. Next to the files it keeps the "base", a copy of
// what the M-SERV looked like at the last pull, so edits can be diffed and
// conflicts detected.
//
//	devices.json          CAN modules as the M-SERV lists them (reference only)
//	config/<table>.json   app configuration tables
//	logic/<mac>.json      decoded rules of one module
//	nodered/<flow>.json   one Node-RED tab or subflow; global.json = config nodes
//	blobs/<hash>.b64      long base64 values (icons, photos), referenced as "blob:<hash>"
//	.ampiocli/base/...    the same tree as last pulled
//	.ampiocli/backups/... the M-SERV's state just before each push
package workdir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Files maps a slash-separated path relative to the working copy to its JSON.
type Files map[string]json.RawMessage

// Copy is a working copy rooted at a folder.
type Copy string

// Tree is one tree of files inside a copy: the work tree, the base, or a backup.
type Tree struct {
	copy Copy
	sub  string // "" for the work tree
}

const meta = ".ampiocli"

func (c Copy) Work() Tree { return Tree{c, ""} }
func (c Copy) Base() Tree { return Tree{c, meta + "/base"} }

const backups = meta + "/backups"

// Backup stores files under .ampiocli/backups/<time>/ and returns that
// folder. A second backup within the same second gets a -2, -3, ... suffix.
func (c Copy) Backup(files Files) (string, error) {
	name := time.Now().Format("2006-01-02T15-04-05")
	t := Tree{c, backups + "/" + name}
	if err := os.MkdirAll(filepath.Dir(t.dir()), 0o755); err != nil {
		return "", err
	}
	for n := 2; ; n++ {
		err := os.Mkdir(t.dir(), 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		t.sub = fmt.Sprintf("%s/%s-%d", backups, name, n)
	}
	return t.dir(), t.Write(files, func(string) bool { return true })
}

// Backups lists the backups by name, oldest first.
func (c Copy) Backups() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(string(c), filepath.FromSlash(backups)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, err
}

// BackupTree returns the backup called name.
func (c Copy) BackupTree(name string) (Tree, error) {
	names, err := c.Backups()
	if err != nil {
		return Tree{}, err
	}
	if !slices.Contains(names, name) {
		return Tree{}, fmt.Errorf("no backup %q (see ampiocli backups)", name)
	}
	return Tree{c, backups + "/" + name}, nil
}

// blobs are shared by every tree (they are content-addressed).
func (c Copy) blobs() string { return filepath.Join(string(c), "blobs") }

func (t Tree) dir() string { return filepath.Join(string(t.copy), filepath.FromSlash(t.sub)) }

func (t Tree) path(rel string) string { return filepath.Join(t.dir(), filepath.FromSlash(rel)) }

// list returns the paths of the JSON files accepted by owns.
func (t Tree) list(owns func(path string) bool) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(t.dir(), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(t.dir(), p)
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if rel == meta || rel == "blobs" || rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, ".json") && owns(rel) {
			paths = append(paths, rel)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return paths, err
}

// Read returns every JSON file accepted by owns.
func (t Tree) Read(owns func(path string) bool) (Files, error) {
	paths, err := t.list(owns)
	if err != nil {
		return nil, err
	}
	files := Files{}
	for _, rel := range paths {
		raw, err := os.ReadFile(t.path(rel))
		if err != nil {
			return nil, err
		}
		if raw, err = t.copy.inlineBlobs(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%s: not valid JSON", rel)
		}
		files[rel] = raw
	}
	return files, nil
}

// Write replaces every file accepted by owns with files. It does not read
// the old files, so it also replaces broken ones.
func (t Tree) Write(files Files, owns func(path string) bool) error {
	old, err := t.list(owns)
	if err != nil {
		return err
	}
	for _, p := range old {
		if _, keep := files[p]; !keep {
			if err := os.Remove(t.path(p)); err != nil {
				return err
			}
		}
	}
	for p, raw := range files {
		var out bytes.Buffer
		if err := json.Indent(&out, raw, "", "  "); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out.WriteByte('\n')
		text, err := t.copy.extractBlobs(out.Bytes())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(t.path(p)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(t.path(p), text, 0o644); err != nil {
			return err
		}
	}
	return nil
}

var (
	longBase64 = regexp.MustCompile(`"([A-Za-z0-9+/]{512,}={0,2})"`)
	blobRef    = regexp.MustCompile(`"blob:([0-9a-f]{16})"`)
)

func (c Copy) extractBlobs(text []byte) ([]byte, error) {
	var err error
	out := longBase64.ReplaceAllFunc(text, func(m []byte) []byte {
		data := m[1 : len(m)-1]
		sum := sha256.Sum256(data)
		name := hex.EncodeToString(sum[:8])
		path := filepath.Join(c.blobs(), name+".b64")
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			if e := os.MkdirAll(c.blobs(), 0o755); e != nil {
				err = e
			} else if e := os.WriteFile(path, data, 0o644); e != nil {
				err = e
			}
		}
		return []byte(`"blob:` + name + `"`)
	})
	return out, err
}

func (c Copy) inlineBlobs(text []byte) ([]byte, error) {
	var err error
	out := blobRef.ReplaceAllFunc(text, func(m []byte) []byte {
		name := string(blobRef.FindSubmatch(m)[1])
		data, e := os.ReadFile(filepath.Join(c.blobs(), name+".b64"))
		if e != nil {
			err = fmt.Errorf("missing blob %s", name)
		}
		return append(append([]byte{'"'}, data...), '"')
	})
	return out, err
}
