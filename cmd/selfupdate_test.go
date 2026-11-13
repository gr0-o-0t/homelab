package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/release"
)

// selfStub is upgrade's CLI step against fakes: a release server (TLS
// httptest), this "binary" a temp file reading "OLD", and sudo and exec
// recorded, never run.
type selfStub struct {
	exe       string
	badSum    atomic.Bool
	api, dl   atomic.Int32
	mu        sync.Mutex
	sudo      [][]string
	execPath  string
	execArgv  []string
	execEnv   []string
	sudoErr   error
	sudoUsed  bool
	whyNoSudo string
}

func (s *selfStub) apiHits() int32 { return s.api.Load() }

func (s *selfStub) content(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.exe)
	require.NoError(t, err)
	return string(b)
}

// assertUntouched: the binary is the old one and no temp file is left.
func (s *selfStub) assertUntouched(t *testing.T) {
	t.Helper()
	assert.Equal(t, "OLD", s.content(t))
	s.assertAlone(t)
}

func (s *selfStub) assertAlone(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(s.exe))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"homelab"}, names, "no temp file left next to the binary")
}

// stubSelfUpgrade publishes latest (and every tag in also) on a fake
// release server, each archive's binary reading "NEW-<tag>", and points the
// CLI at it, at version, and at a temp binary.
func stubSelfUpgrade(t *testing.T, version, latest string, also ...string) *selfStub {
	t.Helper()
	s := &selfStub{}
	dir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	s.exe = filepath.Join(dir, "homelab")
	require.NoError(t, os.WriteFile(s.exe, []byte("OLD"), 0o755)) //nolint:gosec // a fake binary

	var srv *httptest.Server
	mux := http.NewServeMux()
	publish := func(tag string) {
		archive := releaseArchive(t, tag)
		rel := func(w http.ResponseWriter, _ *http.Request) {
			s.api.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag,
				"assets": []map[string]string{
					{"name": release.AssetName(tag), "browser_download_url": srv.URL + "/dl/" + tag + "/archive"},
					{"name": release.SumsAsset, "browser_download_url": srv.URL + "/dl/" + tag + "/sums"},
				},
			})
		}
		if tag == latest {
			mux.HandleFunc("/releases/latest", rel)
		}
		mux.HandleFunc("/releases/tags/"+tag, rel)
		mux.HandleFunc("/dl/"+tag+"/archive", func(w http.ResponseWriter, _ *http.Request) {
			s.dl.Add(1)
			_, _ = w.Write(archive)
		})
		mux.HandleFunc("/dl/"+tag+"/sums", func(w http.ResponseWriter, _ *http.Request) {
			sum := sha256.Sum256(archive)
			if s.badSum.Load() {
				sum = sha256.Sum256([]byte("something else"))
			}
			_, _ = fmt.Fprintf(w, "%x  %s\n", sum, release.AssetName(tag))
		})
	}
	for _, tag := range append([]string{latest}, also...) {
		publish(tag)
	}
	srv = httptest.NewTLSServer(mux)

	prevV, prevC, prevA, prevExe, prevSudo, prevExec, prevUsable, prevUA, prevRename :=
		Version, releaseTransport, releaseAPI, selfExecutable, runSudo, execSelf, sudoUsable, release.UserAgent, renameFile
	t.Cleanup(func() {
		srv.Close()
		Version, releaseTransport, releaseAPI, selfExecutable, runSudo, execSelf, sudoUsable, release.UserAgent, renameFile =
			prevV, prevC, prevA, prevExe, prevSudo, prevExec, prevUsable, prevUA, prevRename
		_ = os.Chmod(dir, 0o755) //nolint:gosec // let TempDir clean up
	})
	Version = version
	releaseTransport = trusting(srv)
	releaseAPI = srv.URL
	selfExecutable = func() (string, error) { return s.exe, nil }
	runSudo = func(args []string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sudo = append(s.sudo, append([]string(nil), args...))
		return s.sudoErr
	}
	execSelf = func(path string, argv, env []string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.execPath, s.execArgv, s.execEnv = path, append([]string(nil), argv...), append([]string(nil), env...)
		return nil
	}
	sudoUsable = func() (string, bool) {
		s.sudoUsed = true
		return s.whyNoSudo, s.whyNoSudo == ""
	}
	return s
}

// releaseArchive is laid out as `make release` builds it.
func releaseArchive(t *testing.T, tag string) []byte {
	t.Helper()
	dir := strings.TrimSuffix(release.AssetName(tag), ".tar.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0o755, Typeflag: tar.TypeDir}))
	body := "NEW-" + tag
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: dir + "/homelab", Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(body))}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// readOnlyDir makes the binary's directory refuse new files.
func readOnlyDir(t *testing.T, s *selfStub) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	require.NoError(t, os.Chmod(filepath.Dir(s.exe), 0o555)) //nolint:gosec // a test dir
}

var v150 = bootstrap.AgentID{Version: "v1.5.0", Boot: "new-container"}

// A newer release: this binary is replaced (writable: in place, no sudo),
// then the agents are rolled to that release's image.
func TestUpgradeCLI_ReplacesWritableBinaryThenRollsAgents(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	var code int
	stderr := captureStderr(func() { code = executeArgs([]string{"upgrade"}) })
	require.Equal(t, 0, code, stderr)

	assert.Equal(t, "NEW-v1.5.0", s.content(t))
	fi, err := os.Stat(s.exe)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
	s.assertAlone(t)
	assert.Equal(t, [][]string{{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}}, *fw)
	assert.Empty(t, s.sudo)
	assert.False(t, s.sudoUsed)
	assert.Empty(t, s.execPath)
	assert.NotContains(t, stderr, "install the v1.5.0 CLI", "this CLI is v1.5.0 now")
}

// --version picks that release, not the latest.
func TestUpgradeCLI_VersionPicksThatRelease(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.6.0", "v1.5.0")
	require.Equal(t, 0, executeArgs([]string{"upgrade", "--version", "v1.5.0"}))
	assert.Equal(t, "NEW-v1.5.0", s.content(t))
	assert.Equal(t, [][]string{{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}}, *fw)

	// …and only a release tag.
	resetFlags()
	*fw = nil
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--version", "dev"}))
	assert.Empty(t, *fw)
}

// A binary the user cannot write is replaced by `sudo -- <exe> upgrade
// --cli-only --version <tag>` (nothing downloaded here), then this process
// execs the new binary to roll the agents as the invoking user — root
// flags kept wherever they stood, the exec'd leg marked with the tag.
func TestUpgradeCLI_UnwritableBinaryGoesThroughSudoThenExecs(t *testing.T) {
	for _, argv := range [][]string{{"--no-color", "upgrade"}, {"upgrade", "--no-color"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			fw, asks, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
			s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
			readOnlyDir(t, s)
			var code int
			stderr := captureStderr(func() { code = executeArgs(argv) })
			require.Equal(t, 0, code, stderr)

			assert.Contains(t, stderr, "homelab: "+s.exe+" is not writable; re-running with sudo…")
			assert.Equal(t, [][]string{{s.exe, "upgrade", "--cli-only", "--version", "v1.5.0"}}, s.sudo)
			assert.Equal(t, s.exe, s.execPath)
			assert.Equal(t, []string{s.exe, "upgrade", "--version", "v1.5.0", "--no-color=true"}, s.execArgv)
			assert.Contains(t, s.execEnv, upgradeSelfEnv+"=v1.5.0")
			assert.Zero(t, s.dl.Load(), "the root step downloads; this one does not")
			assert.Empty(t, *fw, "the exec'd binary rolls the agents")
			assert.Zero(t, *asks)
			s.assertUntouched(t)
		})
	}
}

// A directory that lets a file be created but not renamed over the binary
// (sticky, root-owned binary) takes the sudo path too.
func TestUpgradeCLI_RenameRefusedGoesThroughSudo(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EPERM}
	}
	require.Equal(t, 0, executeArgs([]string{"upgrade"}))
	assert.Equal(t, [][]string{{s.exe, "upgrade", "--cli-only", "--version", "v1.5.0"}}, s.sudo)
	assert.Equal(t, s.exe, s.execPath)
	assert.Empty(t, *fw)
	s.assertUntouched(t)
}

// The binary exec'd after the sudo step: no release lookup, no download,
// no CLI step — straight to the agents with the tag's image.
func TestUpgradeCLI_ExecdLegRollsAgentsOnly(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.5.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.5.0", "v1.6.0", "v1.5.0")
	t.Setenv(upgradeSelfEnv, "v1.5.0")
	require.Equal(t, 0, executeArgs([]string{"upgrade", "--version", "v1.5.0", "--no-color=true"}))
	assert.Equal(t, [][]string{{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}}, *fw)
	assert.Zero(t, s.apiHits())
	assert.Zero(t, s.dl.Load())
	assert.Empty(t, s.sudo)
	assert.Empty(t, s.execPath)
	s.assertUntouched(t)
}

// An exec'd leg whose binary is not the target (the swap did not take)
// stops: never sudo → exec → sudo again.
func TestUpgradeCLI_ExecdLegWrongVersionStops(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	t.Setenv(upgradeSelfEnv, "v1.5.0")
	var code int
	stderr := captureStderr(func() { code = executeArgs([]string{"upgrade", "--version", "v1.5.0"}) })
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "the installed binary reports v1.4.0, not v1.5.0; not retrying")
	assert.Empty(t, s.sudo)
	assert.Empty(t, s.execPath)
	assert.Zero(t, s.apiHits())
	assert.Empty(t, *fw)
	s.assertUntouched(t)
}

func TestUpgradeCLI_UnwritableWithoutSudoNamesThePath(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	s.whyNoSudo = "sudo is not installed"
	readOnlyDir(t, s)
	var code int
	stderr := captureStderr(func() { code = executeArgs([]string{"upgrade"}) })
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, s.exe+" is not writable")
	assert.Contains(t, stderr, "sudo is not installed")
	assert.Contains(t, stderr, "sudo "+s.exe+" upgrade --cli-only --version v1.5.0")
	assert.Empty(t, s.sudo)
	assert.Empty(t, s.execPath)
	assert.Empty(t, *fw)
	s.assertUntouched(t)
}

func TestUpgradeCLI_SudoFailureStops(t *testing.T) {
	fw, _, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	s.sudoErr = errors.New("exit status 1")
	readOnlyDir(t, s)
	assert.Equal(t, 1, executeArgs([]string{"upgrade"}))
	assert.Len(t, s.sudo, 1)
	assert.Empty(t, s.execPath)
	assert.Empty(t, *fw)
}

// A download that does not match SHA256SUMS replaces nothing and rolls no
// agent.
func TestUpgradeCLI_ChecksumFailureReplacesNothing(t *testing.T) {
	fw, asks, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader, v150)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	s.badSum.Store(true)
	var code int
	stderr := captureStderr(func() { code = executeArgs([]string{"upgrade"}) })
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "left unchanged")
	s.assertUntouched(t)
	assert.Empty(t, *fw)
	assert.Zero(t, *asks)
}


// No newer release: the CLI stays, and the rollout is still forwarded —
// the leader leaves alone every agent already on the CLI's version (itself
// included: no marker, so no wait for a restart) and catches up the rest.
func TestUpgradeCLI_NothingNewerStillForwards(t *testing.T) {
	fw, asks, _ := fakeUpgrade(t, "v1.5.0", false, 0, nil, v150, v150)
	s := stubSelfUpgrade(t, "v1.5.0", "v1.5.0")
	out := captureStdout(t, func() { assert.Equal(t, 0, executeArgs([]string{"upgrade"})) })
	assert.Contains(t, out, "this CLI is already v1.5.0")
	assert.NotContains(t, out, "everything is on", "only the leader can tell")
	assert.Equal(t, [][]string{{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.5.0", "--version", "v1.5.0"}}, *fw)
	assert.Equal(t, 2, *asks, "the version check and the probe before; no wait")
	assert.Zero(t, s.dl.Load())
	s.assertUntouched(t)
}

// A CLI newer than the latest release is not "upgraded" to it: the CLI is
// left alone and the agents are rolled to its version; a leader on a newer
// release than this CLI is never downgraded.
func TestUpgradeCLI_NewerThanLatestNeverDowngrades(t *testing.T) {
	v160 := bootstrap.AgentID{Version: "v1.6.0", Boot: "newer-container"}
	fw, _, _ := fakeUpgrade(t, "v1.6.0", true, 0, nil, v150, v150, v160)
	s := stubSelfUpgrade(t, "v1.6.0", "v1.5.0")
	out := captureStdout(t, func() { assert.Equal(t, 0, executeArgs([]string{"upgrade"})) })
	assert.Contains(t, out, "this CLI is already v1.6.0 (the latest release is v1.5.0)")
	assert.Equal(t, [][]string{{"upgrade", "--image", "ghcr.io/gr0-o-0t/homelab:v1.6.0", "--version", "v1.6.0"}}, *fw)
	assert.Zero(t, s.dl.Load())
	s.assertUntouched(t)

	fw, _, _ = fakeUpgrade(t, "v1.6.0", true, 0, nil, bootstrap.AgentID{Version: "v1.7.0", Boot: "x"})
	s = stubSelfUpgrade(t, "v1.6.0", "v1.5.0")
	out = captureStdout(t, func() { assert.Equal(t, 0, executeArgs([]string{"upgrade"})) })
	assert.Contains(t, out, "the leader agent runs v1.7.0, newer than this CLI")
	assert.Empty(t, *fw)
	s.assertUntouched(t)
}

// The hidden step run under sudo: replace the binary and exit — no leader,
// no forwarding, never sudo again.
func TestUpgradeCLIOnly(t *testing.T) {
	_, asks, _ := fakeUpgrade(t, "v1.4.0", true, 0, nil, oldLeader)
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	calls := fakeAgent(t, 0, nil)
	require.Equal(t, 0, executeArgs([]string{"upgrade", "--cli-only", "--version", "v1.5.0"}))
	assert.Equal(t, "NEW-v1.5.0", s.content(t))
	s.assertAlone(t)
	assert.Empty(t, *calls, "not forwarded")
	assert.Zero(t, *asks, "no leader contact")

	resetFlags()
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--cli-only"}), "needs --version")

	resetFlags()
	s = stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	readOnlyDir(t, s)
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--cli-only", "--version", "v1.5.0"}))
	assert.Empty(t, s.sudo)
	assert.False(t, s.sudoUsed)
	s.assertUntouched(t)
}

func TestUpgradeCLIOnly_RefusedInTheAgent(t *testing.T) {
	s := stubSelfUpgrade(t, "v1.4.0", "v1.5.0")
	t.Cleanup(resetFlags)
	t.Setenv("HOMELAB_AGENT", "1")
	assert.Equal(t, 1, executeArgs([]string{"upgrade", "--cli-only", "--version", "v1.5.0"}))
	assert.Zero(t, s.apiHits())
	s.assertUntouched(t)
}
