//go:build gui

// Package gui is the desktop front end, `homelab --gui`, built on Dear ImGui
// via giu. It is compiled only with `-tags gui` (see `make gui`): giu needs
// cgo and the OpenGL/X11 development headers, which the default, pure-Go
// build must not.
//
// Like the terminal dashboard it has no logic of its own: every button is an
// action from internal/actions, run as the homelab CLI command it describes,
// so the GUI cannot disagree with the CLI about what an action does.
//
// Threading: background goroutines write the shared state under mu. Each
// frame starts by copying it into v (snapshot) and is built from v alone.
// Widget callbacks run while the frame is being built and may take mu, so
// nothing may hold mu across building widgets — a locking call made with mu
// held deadlocks the window. All I/O happens off the render thread; a
// goroutine that changes shared state calls g.Update() to get a new frame.
package gui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	g "github.com/AllenDang/giu"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/service"
)

const refreshEvery = 5 * time.Second

// job is one CLI invocation and its output.
type job struct {
	ID      int
	Label   string
	Argv    []string // after the binary, for display
	Lines   []string
	Partial string
	Running bool
	Stream  bool
	Code    int
	Err     string
	Started time.Time
	Ended   time.Time
}

func (j job) ok() bool { return !j.Running && j.Err == "" }

type toast struct {
	Text  string
	Kind  int // 0 info, 1 ok, 2 fail
	Until time.Time
}

const (
	toastInfo = iota
	toastOK
	toastFail
)

// layerAddrs are a service's addresses on one layer.
type layerAddrs struct {
	Layer, Label string
	Addrs        []network.ServiceAddress
}

type setupResult struct {
	Form *setupForm
	Err  string
	Gen  int
}

type choiceResult struct {
	Items []string
	Err   string
}

// shared is the state background goroutines write. Maps and slices in it are
// replaced, never modified in place, so a snapshot of them stays valid.
type shared struct {
	services    []service.Service
	core        []ContainerState
	layers      []string // configured: ts + enabled extensions
	layerObjs   []network.NetworkLayer
	exts        []extState
	backups     []backup.Listing
	backupsErr  string
	discoverErr string
	loaded      bool
	lastRefresh time.Time

	addrs   map[string][]layerAddrs // by service
	setup   map[string]setupResult  // by service, "" = root
	choices map[string]choiceResult // by Input.Source
	jobs    []job
	toasts  []toast
}

type app struct {
	opt Options

	mu      sync.Mutex
	shared  // guarded by mu
	cancels map[int]context.CancelFunc
	nextJob int
	setupN  int

	v  shared // the frame's snapshot; UI thread only
	ui uiState
}

// Run opens the window and blocks until it is closed.
func Run(opt Options) error {
	a := &app{opt: opt, cancels: map[int]context.CancelFunc{}}
	a.ui.init()
	mw := g.NewMasterWindow("homelab", 1360, 860, 0)
	setupTheme(mw)
	// After the window exists: these call g.Update, which needs its context.
	go a.refresh()
	go func() {
		for range time.Tick(refreshEvery) {
			a.refresh()
		}
	}()
	go a.animate()
	mw.Run(a.loop)
	a.stopAll()
	return nil
}

// animate keeps frames coming while something moves: a running job's spinner
// or a toast that must disappear.
func (a *app) animate() {
	for range time.Tick(100 * time.Millisecond) {
		a.mu.Lock()
		live := slices.ContainsFunc(a.jobs, func(j job) bool { return j.Running })
		now := time.Now()
		n := len(a.toasts)
		a.toasts = slices.DeleteFunc(slices.Clone(a.toasts), func(t toast) bool { return now.After(t.Until) })
		expired := n != len(a.toasts)
		a.mu.Unlock()
		if live || expired || n > 0 {
			g.Update()
		}
	}
}

// ── background state ──────────────────────────────────────────────────────────

func (a *app) refresh() {
	svcs, err := a.opt.Discover()
	var core []ContainerState
	if a.opt.Core != nil {
		core = a.opt.Core()
	}
	var layerObjs []network.NetworkLayer
	if a.opt.Layers != nil {
		layerObjs = a.opt.Layers()
	}
	var layerNames []string
	for _, l := range layerObjs {
		layerNames = append(layerNames, l.Name())
	}
	states := map[string]string{}
	for _, c := range core {
		states[c.Name] = c.State
	}
	var exts []extState
	for _, l := range a.opt.AllLayers {
		if l.Name() == "ts" {
			continue
		}
		exts = append(exts, extState{
			Name: l.Name(), Label: l.Label(), Container: l.ContainerName(),
			Enabled: slices.Contains(layerNames, l.Name()), Running: states[l.ContainerName()] == "running",
		})
	}
	bl, berr := backup.List(backup.DefaultDir(a.opt.Root))

	a.mu.Lock()
	a.core, a.layers, a.layerObjs, a.exts = core, layerNames, layerObjs, exts
	a.backups, a.backupsErr = bl, ""
	if berr != nil {
		a.backupsErr = berr.Error()
	}
	a.discoverErr = ""
	if err != nil {
		a.discoverErr = err.Error()
	} else {
		a.services = svcs
	}
	a.loaded, a.lastRefresh = true, time.Now()
	a.mu.Unlock()
	g.Update()
}

// resolveAddrs looks up a service's addresses on each layer it is exposed
// on. Slow (it may shell into containers), so always in the background.
func (a *app) resolveAddrs(name string) {
	go func() {
		a.mu.Lock()
		var svc *service.Service
		for i := range a.services {
			if a.services[i].Name == name {
				s := a.services[i]
				svc = &s
			}
		}
		objs := a.layerObjs
		a.mu.Unlock()
		if svc == nil || a.opt.Env == nil {
			return
		}
		env := a.opt.Env(name)
		var out []layerAddrs
		for _, l := range objs {
			if !svc.On(service.LayerName(l.Name())) {
				continue
			}
			out = append(out, layerAddrs{Layer: l.Name(), Label: l.Label(), Addrs: l.ServiceAddresses(name, env)})
		}
		a.mu.Lock()
		m := maps.Clone(a.addrs)
		if m == nil {
			m = map[string][]layerAddrs{}
		}
		m[name] = out
		a.addrs = m
		a.mu.Unlock()
		g.Update()
	}()
}

// loadSetup reads `setup [svc] --json` into a form.
func (a *app) loadSetup(svc string) {
	go func() {
		argv := append(slices.Clone(a.opt.CLI), loadSetupArgs(svc)...)
		cmd := exec.Command(argv[0], argv[1:]...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		res := setupResult{}
		if err != nil {
			res.Err = strings.TrimSpace(lastLine(stderr.String() + "\n" + err.Error()))
		} else if f, perr := parseSetup(out); perr != nil {
			res.Err = "unreadable setup --json output: " + perr.Error()
		} else {
			res.Form = f
		}
		a.mu.Lock()
		a.setupN++
		res.Gen = a.setupN
		m := maps.Clone(a.setup)
		if m == nil {
			m = map[string]setupResult{}
		}
		m[svc] = res
		a.setup = m
		a.mu.Unlock()
		g.Update()
	}()
}

// loadChoices resolves a dynamic Choice source (groups, backups).
func (a *app) loadChoices(in actions.Input) {
	go func() {
		items, err := actions.Choices(a.opt.Root, in)
		res := choiceResult{Items: items}
		if err != nil {
			res.Err = err.Error()
		}
		a.mu.Lock()
		m := maps.Clone(a.choices)
		if m == nil {
			m = map[string]choiceResult{}
		}
		m[in.Source] = res
		a.choices = m
		a.mu.Unlock()
		g.Update()
	}()
}

func (a *app) notify(text string, kind int) {
	a.mu.Lock()
	a.toasts = append(slices.Clone(a.toasts), toast{Text: text, Kind: kind, Until: time.Now().Add(4 * time.Second)})
	if len(a.toasts) > 4 {
		a.toasts = a.toasts[len(a.toasts)-4:]
	}
	a.mu.Unlock()
	g.Update()
}

// ── jobs ──────────────────────────────────────────────────────────────────────

const maxJobs = 30

// start runs `homelab <args>` in the background, streaming its combined
// output into a job. stdin, when not nil, is written to the process (secrets
// for setup --secrets-stdin). done runs after it exits, off the UI thread.
func (a *app) start(label string, args []string, stdin []byte, stream bool, done func(ok bool)) int {
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.nextJob++
	id := a.nextJob
	a.jobs = append(slices.Clone(a.jobs), job{ID: id, Label: label, Argv: slices.Clone(args), Running: true, Stream: stream, Started: time.Now()})
	for len(a.jobs) > maxJobs {
		i := slices.IndexFunc(a.jobs, func(j job) bool { return !j.Running })
		if i < 0 {
			break
		}
		a.jobs = slices.Delete(a.jobs, i, i+1)
	}
	a.cancels[id] = cancel
	a.mu.Unlock()
	g.Update()

	go func() {
		argv := append(slices.Clone(a.opt.CLI), args...)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		// Own process group, so Stop also ends docker compose under the CLI.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
		cmd.WaitDelay = 3 * time.Second
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		err := cmd.Start()
		if err == nil {
			go func() {
				err := cmd.Wait()
				_ = pw.CloseWithError(err)
				a.finish(id, err, ctx.Err() != nil, label, done)
			}()
			a.pump(id, pr)
			return
		}
		_ = pw.Close()
		a.finish(id, err, false, label, done)
	}()
	return id
}

// pump copies output lines into the job.
func (a *app) pump(id int, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	last := time.Now()
	for sc.Scan() {
		line := cleanLine(sc.Text())
		a.mu.Lock()
		if i := a.jobIndex(id); i >= 0 {
			jobs := slices.Clone(a.jobs)
			jobs[i].Lines = appendCapped(jobs[i].Lines, line)
			a.jobs = jobs
		}
		a.mu.Unlock()
		if time.Since(last) > 50*time.Millisecond {
			last = time.Now()
			g.Update()
		}
	}
	g.Update()
}

func (a *app) jobIndex(id int) int {
	return slices.IndexFunc(a.jobs, func(j job) bool { return j.ID == id })
}

func (a *app) finish(id int, err error, stopped bool, label string, done func(bool)) {
	a.mu.Lock()
	delete(a.cancels, id)
	var stream bool
	var tail string
	if i := a.jobIndex(id); i >= 0 {
		jobs := slices.Clone(a.jobs)
		j := &jobs[i]
		j.Running, j.Ended = false, time.Now()
		stream = j.Stream
		if err != nil && !stopped {
			j.Err = err.Error()
			if ee, ok := err.(*exec.ExitError); ok {
				j.Code = ee.ExitCode()
			}
			for k := len(j.Lines) - 1; k >= 0; k-- {
				if s := strings.TrimSpace(j.Lines[k]); s != "" {
					tail = s
					break
				}
			}
		}
		if stopped {
			j.Lines = appendCapped(j.Lines, "— stopped —")
		}
		a.jobs = jobs
	}
	a.mu.Unlock()
	ok := err == nil || stopped
	switch {
	case stopped:
	case ok && !stream:
		a.notify(label+" — done", toastOK)
	case !ok:
		msg := label + " failed"
		if tail != "" {
			msg += ": " + tail
		}
		a.notify(msg, toastFail)
	}
	if done != nil {
		done(ok)
	}
	if !stream {
		a.refresh()
	}
}

func (a *app) stop(id int) {
	a.mu.Lock()
	c := a.cancels[id]
	a.mu.Unlock()
	if c != nil {
		c()
	}
}

func (a *app) stopAll() {
	a.mu.Lock()
	cs := slices.Collect(maps.Values(a.cancels))
	a.mu.Unlock()
	for _, c := range cs {
		c()
	}
}

func (a *app) clearJob(id int) {
	a.mu.Lock()
	if i := a.jobIndex(id); i >= 0 {
		jobs := slices.Clone(a.jobs)
		jobs[i].Lines = nil
		a.jobs = jobs
	}
	a.mu.Unlock()
}

// clearFinished drops every job that is not running.
func (a *app) clearFinished() {
	a.mu.Lock()
	a.jobs = slices.DeleteFunc(slices.Clone(a.jobs), func(j job) bool { return !j.Running })
	a.mu.Unlock()
}

// ── outside the window ───────────────────────────────────────────────────────

// openTerminal runs the CLI argv in a terminal emulator. ok is false when
// none is installed; the caller then shows the command to copy.
func (a *app) openTerminal(label string, args []string) bool {
	full := append(slices.Clone(a.opt.CLI), args...)
	argv, ok := terminalArgv(full, os.Getenv, exec.LookPath)
	if !ok {
		return false
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		a.notify("Could not open a terminal: "+err.Error(), toastFail)
		return true
	}
	a.notify(label+" — opened in a terminal", toastInfo)
	go func() { _ = cmd.Wait(); a.refresh() }()
	return true
}

// openURL opens a link or directory with the desktop's handler.
func (a *app) openURL(target string) {
	go func() {
		opener := "xdg-open"
		if _, err := exec.LookPath(opener); err != nil {
			opener = "open" // macOS
		}
		if err := exec.Command(opener, target).Run(); err != nil {
			a.notify(fmt.Sprintf("Could not open %s: %v", target, err), toastFail)
		}
	}()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ── snapshot ──────────────────────────────────────────────────────────────────

func (a *app) snapshot() {
	a.mu.Lock()
	a.v = a.shared
	a.v.jobs = slices.Clone(a.jobs)
	a.mu.Unlock()
}
