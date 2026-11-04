// Package models is the canonical model table: what each network name means,
// which weights it names, and how a client must ask for an answer it can
// check.
package models

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Info is what the network agrees a canonical model name means: the weights
// it names and how a client must ask for an answer it can check.
type Info struct {
	Weights  string         `json:"weights_sha256,omitempty"` // bitllm hash-model of the official files; empty = not recorded yet
	Extra    map[string]any `json:"extra,omitempty"`          // fields every canary and every scoring request carries (e.g. thinking off)
	Sampling *Sampling      `json:"sampling,omitempty"`       // set when servers of this model ignore the client's sampling
}

type Sampling struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
}

//go:embed models.json
var modelsJSON []byte

// Manifest is models.json: the README's table of canonical names, in a form
// the node reads.
var Manifest = func() map[string]Info {
	m := map[string]Info{}
	if err := json.Unmarshal(modelsJSON, &m); err != nil {
		panic("models.json: " + err.Error())
	}
	return m
}()

// weightFile reports whether a file in a model directory is weights (and so
// part of its hash). Configs and tokenizers are left out: they change the
// chat template more often than the model.
func weightFile(name string) bool {
	for _, ext := range []string{".safetensors", ".gguf", ".bin", ".pt", ".pth", ".npz"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// HashModel is the sha256 of the lines "<sha256 of file>  <path>\n" for every
// weight file under dir, sorted by path, so the same files give the same hash
// wherever they are stored.
func HashModel(dir string) (string, int, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && weightFile(d.Name()) {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if len(files) == 0 {
		return "", 0, fmt.Errorf("no weight files (.safetensors, .gguf, .bin...) under %s", dir)
	}
	sort.Strings(files)
	all := sha256.New()
	for _, f := range files {
		h := sha256.New()
		r, err := os.Open(filepath.Join(dir, f))
		if err != nil {
			return "", 0, err
		}
		_, err = io.Copy(h, r)
		r.Close()
		if err != nil {
			return "", 0, err
		}
		fmt.Fprintf(all, "%x  %s\n", h.Sum(nil), f)
	}
	return hex.EncodeToString(all.Sum(nil)), len(files), nil
}

// ValidHash reports whether s is a lowercase hex sha256.
func ValidHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
