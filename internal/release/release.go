// Package release finds homelab releases on GitHub, downloads the archive
// matching this build and verifies it against the release's SHA256SUMS.
//
// All network I/O goes through the *http.Client the caller passes; nothing
// is retried. No GitHub token is used.
package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

// DefaultAPI is the GitHub API base of the homelab repository.
const DefaultAPI = "https://api.github.com/repos/gr0-o-0t/homelab"

// SumsAsset is the checksum file published with every release.
const SumsAsset = "SHA256SUMS"

// MaxSize caps a downloaded archive and the binary extracted from it.
const MaxSize = 200 << 20

// maxAPISize caps a release JSON document and SHA256SUMS.
const maxAPISize = 4 << 20

// UserAgent is sent with every request; the CLI sets it to homelab/<version>.
var UserAgent = "homelab"

// Release is one published homelab release.
type Release struct {
	Tag    string
	Assets map[string]string // asset name → browser_download_url
}

type apiRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Latest returns the newest release (GitHub's /releases/latest, which
// excludes prereleases and drafts). api is the repository's API base URL;
// empty means DefaultAPI.
func Latest(ctx context.Context, c *http.Client, api string) (Release, error) {
	return fetchRelease(ctx, c, api, "/releases/latest")
}

// ByTag returns the release tagged tag.
func ByTag(ctx context.Context, c *http.Client, api, tag string) (Release, error) {
	if tag == "" {
		return Release{}, errors.New("release: empty tag")
	}
	return fetchRelease(ctx, c, api, "/releases/tags/"+url.PathEscape(tag))
}

func fetchRelease(ctx context.Context, c *http.Client, api, p string) (Release, error) {
	if api == "" {
		api = DefaultAPI
	}
	body, err := get(ctx, c, strings.TrimRight(api, "/")+p, maxAPISize, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	var r apiRelease
	if err := json.Unmarshal(body, &r); err != nil {
		return Release{}, fmt.Errorf("release: decode %s: %w", p, err)
	}
	if r.TagName == "" {
		return Release{}, fmt.Errorf("release: %s has no tag", p)
	}
	if r.Draft {
		return Release{}, fmt.Errorf("release: %s is a draft", r.TagName)
	}
	rel := Release{Tag: r.TagName, Assets: make(map[string]string, len(r.Assets))}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// AssetName is the archive this build is replaced from: the GUI build takes
// linux_amd64, a -tags nogui build linux_amd64_headless on amd64 and
// linux_arm64 on arm64.
func AssetName(tag string) string {
	return assetName(tag, runtime.GOARCH, guiBuild)
}

func assetName(tag, goarch string, gui bool) string {
	arch := goarch
	if goarch == "amd64" && !gui {
		arch = "amd64_headless"
	}
	return "homelab_" + tag + "_linux_" + arch + ".tar.gz"
}

// FetchBinary downloads this build's archive of rel and SHA256SUMS, checks
// the archive's digest and returns the homelab binary inside it
// (homelab_<tag>_linux_<arch>/homelab). Any mismatch, missing entry,
// oversize or escaping path is an error and returns no bytes.
func FetchBinary(ctx context.Context, c *http.Client, rel Release) ([]byte, error) {
	name := AssetName(rel.Tag)
	archiveURL, ok := rel.Assets[name]
	if !ok {
		return nil, fmt.Errorf("release %s has no asset %s for this build", rel.Tag, name)
	}
	sumsURL, ok := rel.Assets[SumsAsset]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s: refusing an unverified download", rel.Tag, SumsAsset)
	}
	// The sums and the archive share a channel: over plain http the checksum
	// would prove nothing.
	for _, u := range []string{sumsURL, archiveURL} {
		if err := requireHTTPS(u); err != nil {
			return nil, err
		}
	}
	sums, err := get(ctx, c, sumsURL, maxAPISize, "")
	if err != nil {
		return nil, err
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	archive, err := get(ctx, c, archiveURL, MaxSize, "")
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return nil, fmt.Errorf("release %s: %s checksum mismatch (got %x, %s lists %s)",
			rel.Tag, name, got, SumsAsset, want)
	}
	return extract(archive, strings.TrimSuffix(name, ".tar.gz")+"/homelab")
}

// sumFor finds name's digest in sha256sum output ("<hex>  <name>", or
// "<hex> *<name>" in binary mode).
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	var digest string
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if b, err := hex.DecodeString(fields[0]); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("%s: malformed digest for %s", SumsAsset, name)
		}
		if digest != "" {
			return "", fmt.Errorf("%s lists %s more than once", SumsAsset, name)
		}
		digest = fields[0]
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", SumsAsset, err)
	}
	if digest == "" {
		return "", fmt.Errorf("%s has no entry for %s", SumsAsset, name)
	}
	return digest, nil
}

// extract returns the regular file called want from a tar.gz. An entry whose
// path is absolute or climbs out with ".." rejects the whole archive.
func extract(archive []byte, want string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release archive: %w", err)
	}
	defer zr.Close() //nolint:errcheck // in-memory reader
	tr := tar.NewReader(zr)
	var bin []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("release archive: %w", err)
		}
		if unsafePath(h.Name) {
			return nil, fmt.Errorf("release archive: entry %q escapes the archive", h.Name)
		}
		if h.Name != want {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("release archive: %s is not a regular file", want)
		}
		if bin != nil {
			return nil, fmt.Errorf("release archive: %s appears twice", want)
		}
		if h.Size > MaxSize {
			return nil, fmt.Errorf("release archive: %s is over %d bytes", want, MaxSize)
		}
		bin, err = io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("release archive: %s: %w", want, err)
		}
		if int64(len(bin)) != h.Size {
			return nil, fmt.Errorf("release archive: %s is truncated", want)
		}
	}
	if bin == nil {
		return nil, fmt.Errorf("release archive: no %s", want)
	}
	return bin, nil
}

func unsafePath(name string) bool {
	if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func requireHTTPS(u string) error {
	p, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if p.Scheme != "https" {
		return fmt.Errorf("release: refusing %s: not https", u)
	}
	return nil
}

// get fetches u over https only: u itself and, after any redirects, the URL
// that answered.
func get(ctx context.Context, c *http.Client, u string, limit int64, accept string) ([]byte, error) {
	if err := requireHTTPS(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if final := resp.Request.URL; final.Scheme != "https" {
		return nil, fmt.Errorf("release: GET %s: redirected to %s: not https", u, final.Redacted())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: GET %s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("release: GET %s: %w", u, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("release: GET %s: over %d bytes", u, limit)
	}
	return body, nil
}

// DevBuild reports whether current cannot be compared with a release, and
// why: a development build ("dev", v0.0.0-<date>-<sha>, a -dev suffix) or
// no semantic version at all.
func DevBuild(current string) (string, bool) {
	switch {
	case !semver.IsValid(current):
		return fmt.Sprintf("%q is not a release version (a development build)", current), true
	case strings.HasPrefix(current, "v0.0.0-"):
		return current + " is a development build", true
	case strings.HasSuffix(current, "-dev"):
		return current + " is a development build", true
	}
	return "", false
}

// Newer reports whether latest is a newer release than current. A
// development or unparsable current (DevBuild says why), an unparsable
// latest, or a current at or past latest gives false: never a downgrade.
func Newer(current, latest string) bool {
	if _, dev := DevBuild(current); dev || !semver.IsValid(latest) {
		return false
	}
	return semver.Compare(latest, current) > 0
}
