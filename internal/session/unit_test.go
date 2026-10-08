package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/docker"
)

func TestSystemdQuote(t *testing.T) {
	assert.Equal(t, `"/usr/bin/homelab"`, SystemdQuote("/usr/bin/homelab"))
	assert.Equal(t, `"/home/a b/x"`, SystemdQuote("/home/a b/x"))
	assert.Equal(t, `"100%%"`, SystemdQuote("100%"), "specifiers")
	assert.Equal(t, `"$$HOME"`, SystemdQuote("$HOME"), "variable expansion")
	assert.Equal(t, `"a\"b\\c"`, SystemdQuote(`a"b\c`))
}

func TestRenderUnit(t *testing.T) {
	argv := []string{"/opt/my tools/homelab", "--no-color", "--config-dir", "/home/u/.config/homelab", "--config", "/home/u/cfg/50%.yaml", "session", "run"}
	u := RenderUnit(argv, map[string]string{"DOCKER_HOST": "unix:///run/user/1000/docker.sock", "A": "$x"})

	assert.Contains(t, u, `ExecStart="/opt/my tools/homelab" "--no-color" "--config-dir" "/home/u/.config/homelab" "--config" "/home/u/cfg/50%%.yaml" "session" "run"`+"\n")
	assert.Contains(t, u, "Restart=on-failure\n")
	assert.Contains(t, u, "RestartSec=5\n")
	assert.Contains(t, u, "TimeoutStopSec=120\n")
	assert.Contains(t, u, "WantedBy=default.target\n")
	assert.Contains(t, u, `Environment="A=$$x"`+"\n"+`Environment="DOCKER_HOST=unix:///run/user/1000/docker.sock"`+"\n", "sorted, escaped")
	assert.True(t, strings.Index(u, "[Service]") < strings.Index(u, "[Install]"))
}

func TestUnitPath_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	p, err := UnitPath()
	require.NoError(t, err)
	assert.Equal(t, "/x/cfg/systemd/user/homelab.service", p)
}

type fakeSystemctl struct {
	calls     []string
	onRestart func()
	fail      map[string]error
	out       map[string]string
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if (args[0] == "restart" || args[0] == "disable") && f.onRestart != nil {
		f.onRestart()
	}
	return f.out[args[0]], f.fail[args[0]]
}

func newManager(t *testing.T, cs []docker.ContainerInfo) (*Manager, *fakeSystemctl, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	sc := &fakeSystemctl{}
	r := &fakeRunner{}
	return &Manager{
		Root:      root,
		UnitPath:  filepath.Join(t.TempDir(), "systemd", "user", UnitName),
		Argv:      []string{"/bin/homelab", "--config-dir", root, "session", "run"},
		Systemctl: sc.run,
		Docker:    &fakeDocker{cs: cs},
		Run:       r.run,
	}, sc, r
}

func TestManager_InstallAndUninstall(t *testing.T) {
	m, sc, r := newManager(t, nil)
	f := newFixture(m.Root)
	m.Docker = &fakeDocker{cs: []docker.ContainerInfo{
		{Name: "caddy", State: "running", RestartPolicy: "always", Labels: f.core},
		{Name: "jellyfin", State: "running", RestartPolicy: "unless-stopped", Labels: f.svc("jellyfin")},
		{Name: "migrate", State: "exited", RestartPolicy: "no", Labels: f.svc("jellyfin")},
		{Name: "stranger", State: "running", RestartPolicy: "always", Labels: labelsFor("/srv/x")},
	}}
	detachSeen := false
	sc.onRestart = func() { _, err := os.Stat(DetachFile(m.Root)); detachSeen = err == nil }

	require.NoError(t, m.Install(context.Background()))

	unit, err := os.ReadFile(m.UnitPath)
	require.NoError(t, err)
	assert.Contains(t, string(unit), `ExecStart="/bin/homelab"`)
	assert.Equal(t, []string{"daemon-reload", "enable homelab.service", "restart homelab.service"}, sc.calls)
	assert.True(t, detachSeen, "the old supervisor must not stop the stack on reinstall")
	assert.NoFileExists(t, DetachFile(m.Root))
	assert.Equal(t, []string{"docker update --restart=no caddy jellyfin"}, r.Calls(), "only homelab's, only boot-starting")

	d, exists, err := LoadDesired(m.Root)
	require.NoError(t, err)
	assert.True(t, exists, "bootstrapped from the running containers")
	assert.True(t, d.CoreRunning())
	assert.True(t, d.ServiceRunning("jellyfin"))

	// Idempotent: a second install changes no policy (they are "no" now).
	m.Docker = &fakeDocker{cs: []docker.ContainerInfo{
		{Name: "caddy", State: "running", RestartPolicy: "no", Labels: f.core},
		{Name: "jellyfin", State: "running", RestartPolicy: "no", Labels: f.svc("jellyfin")},
		{Name: "migrate", State: "exited", RestartPolicy: "no", Labels: f.svc("jellyfin")},
	}}
	d.SetService("jellyfin", Stopped)
	require.NoError(t, d.Save(m.Root))
	require.NoError(t, m.Install(context.Background()))
	assert.Len(t, r.Calls(), 1, "no docker update the second time")
	d2, _, _ := LoadDesired(m.Root)
	assert.False(t, d2.ServiceRunning("jellyfin"), "an existing desired state is kept")

	// Uninstall: supervisor detached, unit gone, policies back.
	sc.calls = nil
	require.NoError(t, m.Uninstall(context.Background()))
	assert.Equal(t, []string{"disable --now homelab.service", "daemon-reload"}, sc.calls)
	assert.True(t, detachSeen)
	assert.NoFileExists(t, m.UnitPath)
	assert.Equal(t, []string{
		"docker update --restart=no caddy jellyfin",
		"docker update --restart=always caddy",
		"docker update --restart=unless-stopped jellyfin",
	}, r.Calls())
	assert.NoFileExists(t, PolicyFile(m.Root))
}

func TestManager_InstallNeedsDocker(t *testing.T) {
	m, sc, _ := newManager(t, nil)
	m.Docker = &fakeDocker{listErr: errFake}
	require.Error(t, m.Install(context.Background()))
	assert.Empty(t, sc.calls)
	assert.NoFileExists(t, DesiredFile(m.Root), "no empty desired state that would restore nothing")
}

func TestManager_Status(t *testing.T) {
	m, sc, _ := newManager(t, nil)
	f := newFixture(m.Root)
	m.Docker = &fakeDocker{cs: []docker.ContainerInfo{
		{Name: "caddy", State: "running", RestartPolicy: "always", Labels: f.core},
		{Name: "web", State: "running", RestartPolicy: "no", Labels: f.svc("web")},
	}}
	sc.out = map[string]string{"is-active": "inactive", "is-enabled": "disabled"}
	st := m.Status(context.Background())
	assert.False(t, st.Installed)
	assert.False(t, st.ActiveNow())
	assert.Equal(t, "inactive", st.Active)
	assert.Equal(t, []string{"caddy"}, st.BootStarting)
	assert.False(t, st.DesiredFile)
}

func TestDisableRestartPolicies_ForgetsRemovedContainers(t *testing.T) {
	root := t.TempDir()
	f := newFixture(root)
	require.NoError(t, Policies{"gone": "always", "web": "always"}.Save(root))
	cs := []docker.ContainerInfo{{ID: "1", Name: "web", RestartPolicy: "no", Labels: f.svc("web")}}
	r := &fakeRunner{}
	_, err := DisableRestartPolicies(context.Background(), root, NewScope(root), cs, r.run)
	require.NoError(t, err)
	pol, err := LoadPolicies(root)
	require.NoError(t, err)
	assert.Equal(t, Policies{"web": "always"}, pol)

	// An empty listing keeps the record.
	_, err = DisableRestartPolicies(context.Background(), root, NewScope(root), nil, r.run)
	require.NoError(t, err)
	pol, _ = LoadPolicies(root)
	assert.Equal(t, Policies{"web": "always"}, pol)
}
