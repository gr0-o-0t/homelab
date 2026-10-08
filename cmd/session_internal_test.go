package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/session"
)

// runCLI executes argv against a config dir. The docker calls behind it fail
// (TestMain points DOCKER_HOST nowhere); only what was recorded matters here.
func runCLI(t *testing.T, root string, argv ...string) {
	t.Helper()
	resetFlags()
	t.Cleanup(resetFlags)
	rootCmd.SetArgs(append([]string{"--config-dir", root}, argv...))
	_ = rootCmd.Execute()
}

func desiredFixture(t *testing.T) string {
	t.Helper()
	root := makeBatchFixture(t) // services alpha, beta
	require.NoError(t, os.MkdirAll(filepath.Join(root, "core"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "core", "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("vars:\n  DOMAIN:\n    value: example.com\ngroups:\n  media: [alpha, beta]\n"), 0o644))
	old := desiredBootstrap
	desiredBootstrap = func(string) (*session.Desired, error) { return &session.Desired{Core: session.Stopped}, nil }
	t.Cleanup(func() { desiredBootstrap = old })
	return root
}

func loadDesired(t *testing.T, root string) *session.Desired {
	t.Helper()
	d, exists, err := session.LoadDesired(root)
	require.NoError(t, err)
	require.True(t, exists, "desired.yaml written")
	return d
}

func TestDesiredState_LifecycleTransitions(t *testing.T) {
	root := desiredFixture(t)
	steps := []struct {
		argv        []string
		core        session.State
		alpha, beta session.State
	}{
		{[]string{"start", "alpha"}, session.Stopped, session.Running, ""},
		{[]string{"up", "-a"}, session.Stopped, session.Running, session.Running},
		{[]string{"stop", "--group", "media"}, session.Stopped, session.Stopped, session.Stopped},
		{[]string{"restart", "beta"}, session.Stopped, session.Stopped, session.Running},
		{[]string{"up"}, session.Running, session.Stopped, session.Running},
		{[]string{"down", "beta"}, session.Running, session.Stopped, session.Stopped},
		{[]string{"update", "alpha", "beta"}, session.Running, session.Running, session.Running},
		{[]string{"stop"}, session.Stopped, session.Running, session.Running},
		{[]string{"start"}, session.Running, session.Running, session.Running},
		{[]string{"down"}, session.Stopped, session.Running, session.Running},
		{[]string{"restart"}, session.Running, session.Running, session.Running},
		{[]string{"down", "-a"}, session.Running, session.Stopped, session.Stopped},
		{[]string{"update"}, session.Running, session.Stopped, session.Stopped},
		{[]string{"prune", "--keep-volumes", "--yes", "alpha"}, session.Running, "", session.Stopped},
	}
	for _, s := range steps {
		runCLI(t, root, s.argv...)
		d := loadDesired(t, root)
		name := strings.Join(s.argv, " ")
		assert.Equal(t, s.core, d.Core, "%s: core", name)
		assert.Equal(t, s.alpha, d.Services["alpha"], "%s: alpha", name)
		assert.Equal(t, s.beta, d.Services["beta"], "%s: beta", name)
	}
}

func TestDesiredState_DeleteForgetsService(t *testing.T) {
	root := desiredFixture(t)
	runCLI(t, root, "up", "alpha", "beta")
	runCLI(t, root, "delete", "--yes", "--force", "alpha")
	d := loadDesired(t, root)
	_, ok := d.Services["alpha"]
	assert.False(t, ok, "deleted services leave the desired state")
	assert.Equal(t, session.Running, d.Services["beta"])
}

func TestDesiredState_UnknownServiceNotRecorded(t *testing.T) {
	root := desiredFixture(t)
	runCLI(t, root, "up", "nosuch")
	_, exists, err := session.LoadDesired(root)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestDesiredState_NoRecordFlag(t *testing.T) {
	root := desiredFixture(t)
	runCLI(t, root, "--no-record", "stop", "alpha")
	_, exists, _ := session.LoadDesired(root)
	assert.False(t, exists, "the supervisor's own calls leave the desired state alone")
}

func TestDesiredState_NoDockerNoFile(t *testing.T) {
	root := desiredFixture(t)
	desiredBootstrap = func(string) (*session.Desired, error) { return nil, os.ErrNotExist }
	runCLI(t, root, "stop", "alpha")
	_, exists, _ := session.LoadDesired(root)
	assert.False(t, exists, "no file claiming everything else stopped")
}

func TestSessionArgv_PassesConfigSelection(t *testing.T) {
	setConfigDir(t, "rel/dir")
	rootFlags.configFile = "cfg/home lab.yaml"
	argv := sessionArgv("rel/dir")
	require.GreaterOrEqual(t, len(argv), 7)
	assert.True(t, filepath.IsAbs(argv[0]), "binary path absolute: %s", argv[0])
	cwd, _ := os.Getwd()
	assert.Equal(t, []string{"--no-color", "--config-dir", filepath.Join(cwd, "rel/dir"),
		"--config", filepath.Join(cwd, "cfg/home lab.yaml"), "session", "run"}, argv[1:])

	unit := session.RenderUnit(argv, nil)
	assert.Contains(t, unit, `"`+filepath.Join(cwd, "cfg/home lab.yaml")+`"`)
}

type cmdFakeDocker struct{ cs []docker.ContainerInfo }

func (cmdFakeDocker) PingOK(context.Context) error { return nil }
func (f cmdFakeDocker) ComposeContainers(context.Context) ([]docker.ContainerInfo, error) {
	return f.cs, nil
}
func (cmdFakeDocker) InspectInfo(context.Context, string) (docker.ContainerInfo, bool, error) {
	return docker.ContainerInfo{}, false, nil
}
func (cmdFakeDocker) ContainerEvents(context.Context) (<-chan docker.Event, <-chan error) {
	return nil, nil
}

func TestSessionInstallUninstall_WithFakes(t *testing.T) {
	root := desiredFixture(t)
	abs, _ := filepath.Abs(root)
	labels := map[string]string{docker.LabelProject: "core", docker.LabelWorkingDir: filepath.Join(abs, "core")}
	var systemctl, dockerCalls []string
	oldS, oldD, oldR := sessionSystemctl, sessionDocker, sessionRunner
	t.Cleanup(func() { sessionSystemctl, sessionDocker, sessionRunner = oldS, oldD, oldR })
	sessionSystemctl = func(_ context.Context, args ...string) (string, error) {
		systemctl = append(systemctl, strings.Join(args, " "))
		return "active", nil
	}
	cs := []docker.ContainerInfo{{Name: "caddy", State: "running", RestartPolicy: "always", Labels: labels}}
	sessionDocker = func() (session.Docker, func()) { return cmdFakeDocker{cs: cs}, func() {} }
	sessionRunner = func() session.Runner {
		return func(_ context.Context, argv ...string) error {
			dockerCalls = append(dockerCalls, strings.Join(argv, " "))
			return nil
		}
	}

	runCLI(t, root, "session", "install")
	unit, err := sessionUnitPath()
	require.NoError(t, err)
	body, err := os.ReadFile(unit)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"--config-dir" "`+abs+`" "session" "run"`)
	assert.Equal(t, []string{"daemon-reload", "enable homelab.service", "restart homelab.service"}, systemctl)
	assert.Equal(t, []string{"docker update --restart=no caddy"}, dockerCalls)
	assert.True(t, loadDesired(t, root).CoreRunning())

	cs[0].RestartPolicy = "no"
	runCLI(t, root, "session", "uninstall")
	assert.NoFileExists(t, unit)
	assert.Equal(t, "docker update --restart=always caddy", dockerCalls[len(dockerCalls)-1])
}
