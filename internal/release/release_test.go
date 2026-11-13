package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const tag = "v0.5.1"

type entry struct {
	name string
	body string
	typ  byte
}

func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: typ}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// goodArchive is laid out as `make release` builds it.
func goodArchive(t *testing.T) []byte {
	dir := strings.TrimSuffix(AssetName(tag), ".tar.gz")
	return tarGz(t,
		entry{name: dir + "/", typ: tar.TypeDir},
		entry{name: dir + "/CHANGELOG.md", body: "changes"},
		entry{name: dir + "/LICENSE", body: "license"},
		entry{name: dir + "/README.md", body: "readme"},
		entry{name: dir + "/homelab", body: "NEW-BINARY"},
	)
}

func sumsLine(data []byte, name string) string {
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
}

// server publishes one release: its JSON at /releases/latest and
// /releases/tags/<tag>, the archive and SHA256SUMS as assets.
func server(t *testing.T, archive []byte, sums string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	rel := func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "homelab") {
			t.Errorf("User-Agent = %q", ua)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   tag,
			"prerelease": false,
			"draft":      false,
			"assets": []map[string]string{
				{"name": AssetName(tag), "browser_download_url": srv.URL + "/dl/archive"},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/sums"},
			},
		})
	}
	mux.HandleFunc("/repo/releases/latest", rel)
	mux.HandleFunc("/repo/releases/tags/"+tag, rel)
	mux.HandleFunc("/repo/releases/tags/v0.9.0", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.9.0", "draft": true})
	})
	mux.HandleFunc("/dl/archive", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sums)) })
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestParsesTagAndAssets(t *testing.T) {
	srv := server(t, nil, "")
	rel, err := Latest(context.Background(), srv.Client(), srv.URL+"/repo")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != tag {
		t.Errorf("Tag = %q", rel.Tag)
	}
	if rel.Assets[AssetName(tag)] != srv.URL+"/dl/archive" || rel.Assets["SHA256SUMS"] != srv.URL+"/dl/sums" {
		t.Errorf("Assets = %v", rel.Assets)
	}
}

func TestByTag(t *testing.T) {
	srv := server(t, nil, "")
	rel, err := ByTag(context.Background(), srv.Client(), srv.URL+"/repo", tag)
	if err != nil || rel.Tag != tag {
		t.Fatalf("ByTag = %+v, %v", rel, err)
	}
	if _, err := ByTag(context.Background(), srv.Client(), srv.URL+"/repo", "v9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Errorf("unknown tag: err = %v, want a 404", err)
	}
}

func TestFetchBinary(t *testing.T) {
	archive := goodArchive(t)
	srv := server(t, archive, sumsLine([]byte("other"), "homelab_v0.5.1_linux_arm64.tar.gz")+sumsLine(archive, AssetName(tag)))
	rel, err := Latest(context.Background(), srv.Client(), srv.URL+"/repo")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := FetchBinary(context.Background(), srv.Client(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if string(bin) != "NEW-BINARY" {
		t.Errorf("binary = %q", bin)
	}
}

func fetchErr(t *testing.T, archive []byte, sums string) error {
	t.Helper()
	srv := server(t, archive, sums)
	rel, err := Latest(context.Background(), srv.Client(), srv.URL+"/repo")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := FetchBinary(context.Background(), srv.Client(), rel)
	if err == nil {
		t.Fatalf("FetchBinary returned %q, want an error", bin)
	}
	if bin != nil {
		t.Errorf("FetchBinary returned bytes with error %v", err)
	}
	return err
}

func TestFetchBinaryChecksumMismatch(t *testing.T) {
	archive := goodArchive(t)
	err := fetchErr(t, archive, sumsLine([]byte("something else"), AssetName(tag)))
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryTruncatedArchive(t *testing.T) {
	archive := goodArchive(t)
	sums := sumsLine(archive, AssetName(tag))
	err := fetchErr(t, archive[:len(archive)/2], sums)
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryMissingSumsEntry(t *testing.T) {
	archive := goodArchive(t)
	err := fetchErr(t, archive, sumsLine(archive, "homelab_v0.5.1_linux_other.tar.gz"))
	if !strings.Contains(err.Error(), "no entry for "+AssetName(tag)) {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryEscapingEntry(t *testing.T) {
	dir := strings.TrimSuffix(AssetName(tag), ".tar.gz")
	for _, name := range []string{"../homelab", dir + "/../../etc/passwd", "/etc/passwd"} {
		t.Run(name, func(t *testing.T) {
			archive := tarGz(t,
				entry{name: name, body: "evil"},
				entry{name: dir + "/homelab", body: "NEW-BINARY"},
			)
			err := fetchErr(t, archive, sumsLine(archive, AssetName(tag)))
			if !strings.Contains(err.Error(), "escapes") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestFetchBinaryWrongEntryName(t *testing.T) {
	for _, name := range []string{"homelab", "homelab_v0.5.0_linux_amd64/homelab", "./" + strings.TrimSuffix(AssetName(tag), ".tar.gz") + "/homelab"} {
		t.Run(name, func(t *testing.T) {
			archive := tarGz(t, entry{name: name, body: "NEW-BINARY"})
			err := fetchErr(t, archive, sumsLine(archive, AssetName(tag)))
			if !strings.Contains(err.Error(), "release archive: no ") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestFetchBinaryDuplicateSumsEntry(t *testing.T) {
	archive := goodArchive(t)
	err := fetchErr(t, archive, sumsLine(archive, AssetName(tag))+sumsLine([]byte("x"), AssetName(tag)))
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinarySumsOverCap(t *testing.T) {
	err := fetchErr(t, goodArchive(t), strings.Repeat("a", maxAPISize+1))
	if !strings.Contains(err.Error(), "over 4194304 bytes") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryDuplicateBinaryEntry(t *testing.T) {
	dir := strings.TrimSuffix(AssetName(tag), ".tar.gz")
	archive := tarGz(t,
		entry{name: dir + "/homelab", body: "NEW-BINARY"},
		entry{name: dir + "/homelab", body: "OTHER"},
	)
	err := fetchErr(t, archive, sumsLine(archive, AssetName(tag)))
	if !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryBackslashEntry(t *testing.T) {
	dir := strings.TrimSuffix(AssetName(tag), ".tar.gz")
	archive := tarGz(t,
		entry{name: `..\homelab`, body: "evil"},
		entry{name: dir + "/homelab", body: "NEW-BINARY"},
	)
	err := fetchErr(t, archive, sumsLine(archive, AssetName(tag)))
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("err = %v", err)
	}
}

// Plain http for either asset: the checksum would travel the same unprotected
// channel as the archive, so nothing is fetched.
func TestFetchBinaryRefusesPlainHTTPAsset(t *testing.T) {
	var hits int
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(plain.Close)
	for _, which := range []string{"archive", "sums"} {
		rel := Release{Tag: tag, Assets: map[string]string{
			AssetName(tag): "https://example.invalid/archive",
			SumsAsset:      "https://example.invalid/sums",
		}}
		if which == "archive" {
			rel.Assets[AssetName(tag)] = plain.URL + "/archive"
		} else {
			rel.Assets[SumsAsset] = plain.URL + "/sums"
		}
		bin, err := FetchBinary(context.Background(), plain.Client(), rel)
		if err == nil || bin != nil || !strings.Contains(err.Error(), "not https") {
			t.Errorf("%s over http: bin=%q err=%v", which, bin, err)
		}
	}
	if hits != 0 {
		t.Errorf("plain http server was hit %d times", hits)
	}
}

// An https asset redirecting to plain http is refused after the redirect.
func TestFetchBinaryRefusesRedirectToHTTP(t *testing.T) {
	archive := goodArchive(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sumsLine(archive, AssetName(tag))))
	}))
	t.Cleanup(plain.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive" {
			_, _ = w.Write(archive)
			return
		}
		http.Redirect(w, r, plain.URL+"/sums", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	rel := Release{Tag: tag, Assets: map[string]string{
		AssetName(tag): srv.URL + "/archive",
		SumsAsset:      srv.URL + "/sums",
	}}
	bin, err := FetchBinary(context.Background(), srv.Client(), rel)
	if err == nil || bin != nil || !strings.Contains(err.Error(), "redirected to http://") {
		t.Errorf("bin=%q err=%v", bin, err)
	}
}

func TestByTagRefusesDraft(t *testing.T) {
	srv := server(t, nil, "")
	if _, err := ByTag(context.Background(), srv.Client(), srv.URL+"/repo", "v0.9.0"); err == nil ||
		!strings.Contains(err.Error(), "draft") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryEntryNotRegular(t *testing.T) {
	dir := strings.TrimSuffix(AssetName(tag), ".tar.gz")
	archive := tarGz(t, entry{name: dir + "/homelab", typ: tar.TypeSymlink})
	err := fetchErr(t, archive, sumsLine(archive, AssetName(tag)))
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchBinaryNoAsset(t *testing.T) {
	_, err := FetchBinary(context.Background(), http.DefaultClient, Release{Tag: tag, Assets: map[string]string{}})
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Errorf("err = %v", err)
	}
	_, err = FetchBinary(context.Background(), http.DefaultClient,
		Release{Tag: tag, Assets: map[string]string{AssetName(tag): "https://127.0.0.1:0/never"}})
	if err == nil || !strings.Contains(err.Error(), "no SHA256SUMS") {
		t.Errorf("err = %v", err)
	}
}

func TestAssetName(t *testing.T) {
	for _, c := range []struct {
		arch string
		gui  bool
		want string
	}{
		{"amd64", true, "homelab_v0.5.1_linux_amd64.tar.gz"},
		{"amd64", false, "homelab_v0.5.1_linux_amd64_headless.tar.gz"},
		{"arm64", false, "homelab_v0.5.1_linux_arm64.tar.gz"},
		{"arm64", true, "homelab_v0.5.1_linux_arm64.tar.gz"},
	} {
		if got := assetName(tag, c.arch, c.gui); got != c.want {
			t.Errorf("assetName(%s, gui=%v) = %s, want %s", c.arch, c.gui, got, c.want)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"v0.5.0", "v0.5.1", true},
		{"v0.5.1", "v0.5.0", false},
		{"v0.5.1", "v0.5.1", false},
		{"v0.5.1-rc.1", "v0.5.1", true},
		{"v0.5.1", "v0.6.0-rc.1", true},
		{"v0.4.3", "v0.5.0", true},
		{"dev", "v0.5.1", false},
		{"v0.0.0-20261113-abc1234", "v0.5.1", false},
		{"v0.5.1-dev", "v0.6.0", false},
		{"", "v0.5.1", false},
		{"v0.5.0", "garbage", false},
	} {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestDevBuild(t *testing.T) {
	for _, v := range []string{"dev", "v0.0.0-20261113-abc1234", "v0.5.1-dev", "1.2.3"} {
		if reason, dev := DevBuild(v); !dev || reason == "" {
			t.Errorf("DevBuild(%q) = %q, %v; want a reason", v, reason, dev)
		}
	}
	for _, v := range []string{"v0.5.1", "v0.5.1-rc.1"} {
		if reason, dev := DevBuild(v); dev {
			t.Errorf("DevBuild(%q) = %q, true", v, reason)
		}
	}
}

// A 403/429 with no requests left says so, and when it resets (local time).
func TestRateLimit(t *testing.T) {
	reset := time.Date(2026, 11, 13, 15, 4, 5, 0, time.UTC)
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(code)
		}))
		_, err := Latest(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if err == nil {
			t.Fatalf("%d: no error", code)
		}
		want := "GitHub API rate limit reached; retry after " + reset.Local().Format(rateLimitLayout)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%d: %v, want %q", code, err, want)
		}
	}

	// A 403 with requests left is a plain refusal.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "12")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := Latest(context.Background(), srv.Client(), srv.URL)
	if err == nil || strings.Contains(err.Error(), "rate limit") {
		t.Errorf("got %v, want a plain 403", err)
	}
}
