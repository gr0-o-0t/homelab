package cmd

import (
	"context"
	"encoding/json"
	"errors"
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
// inspectErr, when set, fails every inspect.
type fakeSaver struct {
	images     map[string]image.InspectResponse
	inspectErr error
}

func (f fakeSaver) ImageInspectWithRaw(_ context.Context, ref string) (image.InspectResponse, []byte, error) {
	if f.inspectErr != nil {
		return image.InspectResponse{}, nil, f.inspectErr
	}
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
// load answers loaded, or loadCode) and this PC's engine. imagesCode or
// imagesOut make the images --json call fail or answer something else.
type shipStub struct {
	infos      []imageInfo
	imagesCode int
	imagesOut  string
	loaded     map[string]string // ref → identity the leader reports after a load
	loadCode   int
	calls      []leaderCall
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
			switch {
			case s.imagesCode != 0:
				return s.imagesCode, nil
			case s.imagesOut != "":
				_, err := io.WriteString(stdout, s.imagesOut)
				return 0, err
			}
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

func TestShip_ImagesQueryFailsStillForwards(t *testing.T) {
	s := &shipStub{imagesCode: 1}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": localImage("a")}, s)

	assert.Equal(t, 0, executeArgs([]string{"up", "taash", "bogus"}))
	require.Len(t, s.calls, 1, "no load")
	assert.Equal(t, [][]string{{"up", "taash", "bogus"}}, *fwd, "up still reports each service itself")
}

func TestShip_ImagesNotJSONStillForwards(t *testing.T) {
	s := &shipStub{imagesOut: "CONTAINER  IMAGE\nserver     taash-server:current\n"}
	fwd := stubShip(t, map[string]image.InspectResponse{"taash-server:current": localImage("a")}, s)

	assert.Equal(t, 0, executeArgs([]string{"update", "taash"}))
	require.Len(t, s.calls, 1, "an older agent's table: no load")
	assert.Equal(t, [][]string{{"update", "taash"}}, *fwd)
}

func TestShip_LocalInspectErrorSkipsRef(t *testing.T) {
	s := &shipStub{infos: []imageInfo{taashInfo("")}}
	fwd := stubShip(t, nil, s)
	localImages = func() (imageSaver, error) {
		return fakeSaver{inspectErr: errors.New("permission denied")}, nil
	}

	assert.Equal(t, 0, executeArgs([]string{"up", "taash"}))
	require.Len(t, s.calls, 1)
	assert.Equal(t, [][]string{{"up", "taash"}}, *fwd)
}

// up/update warn on a local image whose name has a namespace or registry
// host, once per ref, whether or not it ships; a bare local name does not.
func TestShip_WarnsQualifiedLocalImage(t *testing.T) {
	q := imageInfo{Service: "taash", Container: "worker", Ref: "ghcr.io/me/taash:dev", Local: true, Identity: "x"}
	s := &shipStub{infos: []imageInfo{taashInfo("x"), q, q, {Service: "taash", Container: "web", Ref: "me/nginx:1"}}}
	stubShip(t, nil, s)

	var code int
	out := captureStderr(func() { code = executeArgs([]string{"update", "taash"}) })
	assert.Equal(t, 0, code)
	want := "warning: local image ghcr.io/me/taash:dev has a namespace or registry host; a registry image of that name would replace it\n"
	assert.Equal(t, 1, strings.Count(out, want), "got %q", out)
	assert.NotContains(t, out, "taash-server:current", "a bare local name is not warned about")
	assert.NotContains(t, out, "me/nginx", "a registry image is not warned about")
}
