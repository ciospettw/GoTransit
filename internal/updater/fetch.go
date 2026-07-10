// Package updater keeps a running engine in sync with its remote sources:
// GTFS zips via ETag conditional GET, OSM via Geofabrik osmChange replication.
// Ephemeral by design: remote data lives in RAM, nothing is written to disk
// (the OSM extract only touches a temp file during the initial parse).
// Local file sources are the exception — used in place, never deleted.
package updater

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// httpClient returns a client, optionally tolerant of broken TLS.
func httpClient(insecure bool) *http.Client {
	c := &http.Client{Timeout: 15 * time.Minute}
	if insecure {
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			Proxy:           http.ProxyFromEnvironment,
		}
	}
	return c
}

// CondResult is one conditional in-memory download.
type CondResult struct {
	Changed  bool
	Data     []byte // nil when unchanged
	ETag     string
	LastMod  string
	SHA256   string
	Ignored  bool // server returned 200 with identical content: conditionals ignored
	Duration time.Duration
}

// FetchBytesCond downloads url into memory with If-None-Match /
// If-Modified-Since; a SHA-256 comparison catches servers that ignore them.
// hdr entries (may be nil) are attached to the request: private upstreams.
func FetchBytesCond(url string, insecure bool, etag, lastMod, oldSHA string, hdr map[string]string) (CondResult, error) {
	t0 := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return CondResult{}, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}
	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return CondResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return CondResult{Changed: false, ETag: etag, LastMod: lastMod, SHA256: oldSHA, Duration: time.Since(t0)}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return CondResult{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<30))
	if err != nil {
		return CondResult{}, err
	}
	sum := sha256.Sum256(data)
	res := CondResult{
		Data: data, Changed: true,
		ETag: resp.Header.Get("ETag"), LastMod: resp.Header.Get("Last-Modified"),
		SHA256: hex.EncodeToString(sum[:]), Duration: time.Since(t0),
	}
	if res.SHA256 == oldSHA {
		res.Changed = false
		res.Data = nil
		res.Ignored = etag != "" || lastMod != ""
	}
	return res, nil
}

// FetchBytes downloads a small resource fully in memory, unconditionally.
func FetchBytes(url string, insecure bool) ([]byte, error) {
	resp, err := httpClient(insecure).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// FetchToTemp streams a large resource (the OSM extract) to a temp file.
// The caller parses it and deletes it: the only disk touch in the pipeline.
func FetchToTemp(url string, insecure bool) (string, int64, error) {
	resp, err := httpClient(insecure).Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp("", "gotransit-*.osm.pbf")
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), n, nil
}
