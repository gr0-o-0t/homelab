package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
)

// fakeSaver is this PC's engine: the images it has, by ref.
type fakeSaver struct{ images map[string]image.InspectResponse }

func (f fakeSaver) ImageInspectWithRaw(_ context.Context, ref string) (image.InspectResponse, []byte, error) {
	img, ok := f.images[ref]
	if !ok {
		return image.InspectResponse{}, nil, fmt.Errorf("no such image %s: %w", ref, errdefs.ErrNotFound)
	}
	return img, nil, nil
}

func (f fakeSaver) ImageSave(_ context.Context, refs []string, _ ...client.ImageSaveOption) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("tar:" + strings.Join(refs, ","))), nil
}

func localImage(layer string) image.InspectResponse {
	return image.InspectResponse{
		RootFS:  image.RootFS{Layers: []string{"sha256:" + layer}},
		Created: "2026-11-12T10:00:00Z",
		Size:    42 * 1000 * 1000,
	}
}

// stubShip stands in for the leader (images --json answers infos; images
// load answers loaded, or loadCode) and this PC's engine.
type shipStub struct {
	infos    []imageInfo
	loaded   map[string]string // ref → identity the leader reports after a load
	loadCode int
	calls    []leaderCall
}

func stubShip(t *testing.T, pc map[string]image.InspectResponse, s *shipStub) *[][]string {
	t.Helper()
	t.Setenv("HOMELAB_AGENT", "")
	fwd := fakeAgent(t, 0, nil)
	pl, pi := shipLeader, localImages
	shipLeader = func(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
		c := leaderCall{argv: argv}
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			c.stdin = string(b)
		}
		s.calls = append(s.calls, c)
		if argv[len(argv)-1] == "--json" {
			return 0, json.NewEncoder(stdout).Encode(s.infos)
		}
		if s.loadCode != 0 {
			return s.loadCode, nil
		}
		for _, ref := range argv[2:] {
			_ = json.NewEncoder(stdout).Encode(map[string]string{"ref": ref, "identity": s.loaded[ref]})
		}
		return 0, nil
	}
	localImages = func() (imageSaver, error) { return fakeSaver{images: pc}, nil }
	t.Cleanup(func() { shipLeader, localImages = pl, pi })
	return fwd
}

func taashInfo(identity string) imageInfo {
	return imageInfo{Service: "taash", Container: "server", Ref: "taash-server:current", Local: true, Identity: identity}
}

func TestShip_UpShipsWhenLeaderLacks(t *testing.T) {
	img := localImage("a")
	s := &shipStub{infos: []imageInfo{taashInfo(""), {Service: "taash", Container: "web", Ref: "nginx:1"}},
		loaded: map[string]string{"taash-server:current": swarm.ImageIdentity(img)}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": img}, s)

	assert.Equal(t, 0, executeArgs([]string{"up", "taash"}))
	require.Len(t, s.calls, 2)
	assert.Equal(t, []string{"images", "taash", "--json"}, s.calls[0].argv)
	assert.Equal(t, []string{"images", "load", "taash-server:current"}, s.calls[1].argv)
	assert.Equal(t, "tar:taash-server:current", s.calls[1].stdin)
	assert.Equal(t, [][]string{{"up", "taash"}}, *fwd)
}

func TestShip_UpSkipsWhenLeaderHas(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo(swarm.ImageIdentity(localImage("old")))}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": localImage("new")}, s)

	assert.Equal(t, 0, executeArgs([]string{"up", "taash"}))
	require.Len(t, s.calls, 1, "up never ships a rebuilt image over the leader's (R53)")
	assert.Equal(t, [][]string{{"up", "taash"}}, *fwd)
}

func TestShip_UpdateShipsWhenDiffers(t *testing.T) {
	img := localImage("new")
	s := &shipStub{infos: []imageInfo{taashInfo(swarm.ImageIdentity(localImage("old")))},
		loaded: map[string]string{"taash-server:current": swarm.ImageIdentity(img)}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": img}, s)

	assert.Equal(t, 0, executeArgs([]string{flagNoColor, "update", "taash"}))
	require.Len(t, s.calls, 2)
	assert.Equal(t, []string{flagNoColor, "images", "taash", "--json"}, s.calls[0].argv)
	assert.Equal(t, []string{flagNoColor, "images", "load", "taash-server:current"}, s.calls[1].argv)
	assert.Equal(t, [][]string{{flagNoColor, "update", "taash"}}, *fwd)
}

func TestShip_SkipsWhenIdentityMatches(t *testing.T) {
	img := localImage("a")
	s := &shipStub{infos: []imageInfo{taashInfo(swarm.ImageIdentity(img))}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": img}, s)

	assert.Equal(t, 0, executeArgs([]string{"update", "taash"}))
	require.Len(t, s.calls, 1)
	assert.Equal(t, [][]string{{"update", "taash"}}, *fwd)
}

func TestShip_SkipsWhenPCLacksImage(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo("")}}
	fwd := stubShip(t, nil, s)

	assert.Equal(t, 0, executeArgs([]string{"up", "taash"}))
	require.Len(t, s.calls, 1)
	assert.Equal(t, [][]string{{"up", "taash"}}, *fwd)
}

func TestShip_SkipsWhenPCHasNoDocker(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo("")}}
	fwd := stubShip(t, nil, s)
	localImages = func() (imageSaver, error) { return nil, fmt.Errorf("no docker here") }

	assert.Equal(t, 0, executeArgs([]string{"up", "taash"}))
	require.Len(t, s.calls, 1)
	assert.Equal(t, [][]string{{"up", "taash"}}, *fwd)
}

func TestShip_AllServices(t *testing.T) {
	img := localImage("a")
	shared := imageInfo{Service: "taash-worker", Container: "worker", Ref: "taash-server:current", Local: true}
	s := &shipStub{infos: []imageInfo{taashInfo(""), shared},
		loaded: map[string]string{"taash-server:current": swarm.ImageIdentity(img)}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": img}, s)

	assert.Equal(t, 0, executeArgs([]string{"up", "-a"}))
	require.Len(t, s.calls, 2, "a ref two services share ships once")
	assert.Equal(t, []string{"images", "-a", "--json"}, s.calls[0].argv)
	assert.Equal(t, []string{"images", "load", "taash-server:current"}, s.calls[1].argv)
	assert.Equal(t, [][]string{{"up", "-a"}}, *fwd)
}

func TestShip_GroupFlagPassed(t *testing.T) {
	s := &shipStub{infos: []imageInfo{}}
	stubShip(t, nil, s)

	assert.Equal(t, 0, executeArgs([]string{"update", "--group", "apps"}))
	require.Len(t, s.calls, 1)
	assert.Equal(t, []string{"images", "--group", "apps", "--json"}, s.calls[0].argv)
}

func TestShip_NoTargetForwardsWithoutAsking(t *testing.T) {
	s := &shipStub{}
	fwd := stubShip(t, nil, s)

	assert.Equal(t, 0, executeArgs([]string{"up"}))
	assert.Empty(t, s.calls)
	assert.Equal(t, [][]string{{"up"}}, *fwd)
}

func TestShip_LoadFailureStops(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo("")}, loadCode: 1}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": localImage("a")}, s)

	code, err := forwardDeployFor(t, []string{"up", "taash"})
	assert.NotEqual(t, 0, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "taash-server:current")
	assert.Empty(t, *fwd, "up is not forwarded after a failed ship")
}

func TestShip_IdentityMismatchAfterLoad(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo("")},
		loaded: map[string]string{"taash-server:current": swarm.ImageIdentity(localImage("other"))}}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": localImage("a")}, s)

	code, err := forwardDeployFor(t, []string{"up", "taash"})
	assert.NotEqual(t, 0, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "taash-server:current")
	assert.Empty(t, *fwd)
}

// forwardDeployFor parses argv as Execute would, then runs forwardDeploy
// on it, so the error itself can be checked.
func forwardDeployFor(t *testing.T, argv []string) (int, error) {
	t.Helper()
	c, rest, err := rootCmd.Find(argv)
	require.NoError(t, err)
	require.NoError(t, c.ParseFlags(rest))
	return forwardDeploy(argv)
}

// flagNoColor is a root flag: the leader calls carry it as the argv did.
const flagNoColor = "--no-color"
