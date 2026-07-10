package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Cache is the optional on-disk copy of remote sources ([cache] dir in the
// config). It exists for one reason: a warm restart should revalidate with a
// conditional GET and reuse what it already has, not re-download a 380 MB
// extract that didn't change. Empty Dir = disabled, fully ephemeral.
//
// Layout: <dir>/<name> holds the payload, <dir>/<name>.meta.json the source
// URL and validators (ETag / Last-Modified / SHA-256). A meta whose URL no
// longer matches the config is ignored, so switching sources never serves
// stale data.
type Cache struct {
	Dir string
}

// Enabled reports whether caching is configured.
func (c Cache) Enabled() bool { return c.Dir != "" }

func (c Cache) path(name string) string { return filepath.Join(c.Dir, name) }

// CacheMeta are the stored validators for one cached resource.
type CacheMeta struct {
	URL     string `json:"url"`
	ETag    string `json:"etag,omitempty"`
	LastMod string `json:"last_modified,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// Meta loads the validators for name, if present and still describing url.
func (c Cache) Meta(name, url string) (CacheMeta, bool) {
	if !c.Enabled() {
		return CacheMeta{}, false
	}
	raw, err := os.ReadFile(c.path(name) + ".meta.json")
	if err != nil {
		return CacheMeta{}, false
	}
	var m CacheMeta
	if json.Unmarshal(raw, &m) != nil || m.URL != url {
		return CacheMeta{}, false
	}
	return m, true
}

func (c Cache) writeMeta(name string, m CacheMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path(name)+".meta.json", raw, 0o644)
}

// FilePath returns the cached payload path for name, if the file exists.
func (c Cache) FilePath(name string) (string, bool) {
	if !c.Enabled() {
		return "", false
	}
	p := c.path(name)
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return "", false
	}
	return p, true
}

// LoadBytes reads the cached payload for name.
func (c Cache) LoadBytes(name string) ([]byte, bool) {
	p, ok := c.FilePath(name)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return data, true
}

// StoreBytes atomically persists a payload and its validators.
func (c Cache) StoreBytes(name, url string, data []byte, etag, lastMod, sha string) error {
	if !c.Enabled() {
		return nil
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	tmp := c.path(name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path(name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return c.writeMeta(name, CacheMeta{URL: url, ETag: etag, LastMod: lastMod, SHA256: sha})
}

// AdoptFile atomically moves an already-downloaded file (e.g. a temp PBF)
// into the cache and persists its validators. Returns the cached path.
func (c Cache) AdoptFile(name, url, srcPath, etag, lastMod string) (string, error) {
	if !c.Enabled() {
		return srcPath, nil
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}
	dst := c.path(name)
	if err := os.Rename(srcPath, dst); err != nil {
		// cross-device fallback: copy + remove
		if cerr := copyFile(srcPath, dst); cerr != nil {
			return "", fmt.Errorf("cache adopt: %v (rename: %v)", cerr, err)
		}
		os.Remove(srcPath)
	}
	if err := c.writeMeta(name, CacheMeta{URL: url, ETag: etag, LastMod: lastMod}); err != nil {
		return "", err
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst + ".tmp")
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst + ".tmp")
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}

// FetchToTempCond streams a large resource to a temp file with conditional
// headers. On 304 Not Modified it downloads nothing and returns changed=false.
func FetchToTempCond(url string, insecure bool, etag, lastMod string) (path string, n int64, changed bool, etagOut, lastModOut string, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", 0, false, "", "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}
	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return "", 0, false, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return "", 0, false, etag, lastMod, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, false, "", "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp("", "gotransit-*.osm.pbf")
	if err != nil {
		return "", 0, false, "", "", err
	}
	n, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, false, "", "", err
	}
	return f.Name(), n, true, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), nil
}
