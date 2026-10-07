package dashboard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/tui/styles"
)

// ── action input form ─────────────────────────────────────────────────────────

// field collects one actions.Input.
type field struct {
	in      actions.Input
	ti      textinput.Model // Text and Path
	on      bool            // Bool
	choices []string        // Choice; "" is "none" for an optional one
	ci      int
	loading bool // dynamic choices not resolved yet
	initial string
}

func (f field) value() string {
	switch f.in.Kind {
	case actions.Bool:
		if f.on {
			return "true"
		}
		return ""
	case actions.Choice:
		if f.ci >= 0 && f.ci < len(f.choices) {
			return f.choices[f.ci]
		}
		return f.initial
	}
	return strings.TrimSpace(f.ti.Value())
}

// form is the dialog for an action's Inputs, prefilled with defaults and
// whatever the selection implies.
type form struct {
	p      pending
	fields []field
	focus  int
	err    string
}

func newTextInput(value, placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = 4096
	ti.SetValue(value)
	ti.PromptStyle = styles.Primary
	ti.TextStyle = styles.Text
	ti.PlaceholderStyle = styles.Muted
	return ti
}

// newForm builds the form for p; values in p.inputs override defaults.
func newForm(p pending) form {
	f := form{p: p}
	for _, in := range p.action.Inputs {
		v, ok := p.inputs[in.Key]
		if !ok {
			v = in.Default
		}
		fd := field{in: in, initial: v}
		switch in.Kind {
		case actions.Bool:
			fd.on = v == "true"
		case actions.Choice:
			fd.loading = in.Source != ""
			fd.choices = choiceList(in, in.Choices, v)
			fd.ci = max(slices.Index(fd.choices, v), 0)
		default:
			fd.ti = newTextInput(v, in.Help)
		}
		f.fields = append(f.fields, fd)
	}
	return f.focusField(0)
}

// choiceList is the options of a Choice input: "" first when it is optional,
// and the current value kept even when the list does not have it.
func choiceList(in actions.Input, opts []string, current string) []string {
	var out []string
	if !in.Required {
		out = append(out, "")
	}
	out = append(out, opts...)
	if current != "" && !slices.Contains(out, current) {
		out = append(out, current)
	}
	return out
}

// setChoices installs resolved dynamic choices for the input with key.
func (f form) setChoices(key string, opts []string) form {
	for i := range f.fields {
		fd := &f.fields[i]
		if fd.in.Key != key {
			continue
		}
		cur := fd.value()
		fd.loading = false
		fd.choices = choiceList(fd.in, opts, cur)
		fd.ci = max(slices.Index(fd.choices, cur), 0)
		if cur == "" && fd.in.Required && len(fd.choices) > 0 {
			fd.ci = 0
		}
	}
	return f
}

func (f form) focusField(i int) form {
	if len(f.fields) == 0 {
		return f
	}
	i = (i + len(f.fields)) % len(f.fields)
	for j := range f.fields {
		if f.fields[j].in.Kind == actions.Text || f.fields[j].in.Kind == actions.Path {
			if j == i {
				f.fields[j].ti.Focus()
			} else {
				f.fields[j].ti.Blur()
			}
		}
	}
	f.focus = i
	return f
}

// values are the collected inputs; an error names a missing required one.
func (f form) values() (actions.Inputs, error) {
	in := actions.Inputs{}
	for k, v := range f.p.inputs {
		in[k] = v
	}
	for i := range f.fields {
		fd := &f.fields[i]
		v := fd.value()
		if fd.in.Required && v == "" {
			return nil, fmt.Errorf("%s is required", fd.in.Label)
		}
		in[fd.in.Key] = v
	}
	return in, nil
}

// ── confirmations ─────────────────────────────────────────────────────────────

// confirmState is the yes/no or type-the-name dialog before a dangerous run.
type confirmState struct {
	p     pending
	token string // for TypeName
	ti    textinput.Model
	err   string
}

// ── output ────────────────────────────────────────────────────────────────────

// outputState shows a command's captured output, scrollable.
type outputState struct {
	res result
	vp  viewport.Model
}

// ── setup form ────────────────────────────────────────────────────────────────

// setupInfo mirrors the document `homelab setup [service] --json` prints.
type setupInfo struct {
	Service string `json:"service"`
	Vars    []struct {
		Name        string `json:"name"`
		Value       string `json:"value"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
	} `json:"vars"`
	Secrets []struct {
		Name        string `json:"name"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
		Set         bool   `json:"set"`
		Generated   bool   `json:"generated"`
	} `json:"secrets"`
}

// setupField is a variable (plain text) or a secret (masked, never shown;
// empty keeps what the keyring holds).
type setupField struct {
	name, desc       string
	secret, required bool
	set, generated   bool
	orig             string
	ti               textinput.Model
}

// setupForm edits a service's (or, with svc "", the root config's) vars and
// secrets without the interactive wizard. Vars are saved with --set, secrets
// as JSON on stdin with --secrets-stdin, so no secret value is ever on argv.
type setupForm struct {
	p       pending // service.setup or global.setup, for results and the wizard
	svc     string
	loading bool
	err     string
	fields  []setupField
	focus   int
	top     int
}

func newSetupForm(svc string, info setupInfo) setupForm {
	s := setupForm{svc: svc}
	for _, v := range info.Vars {
		s.fields = append(s.fields, setupField{name: v.Name, desc: v.Description, required: v.Required,
			orig: v.Value, ti: newTextInput(v.Value, v.Description)})
	}
	for _, sec := range info.Secrets {
		ph := "not set"
		switch {
		case sec.Generated && sec.Set:
			ph = "generated — leave empty to keep"
		case sec.Generated:
			ph = "generated on first up"
		case sec.Set:
			ph = "set — leave empty to keep"
		}
		ti := newTextInput("", ph)
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
		s.fields = append(s.fields, setupField{name: sec.Name, desc: sec.Description, secret: true,
			required: sec.Required, set: sec.Set, generated: sec.Generated, ti: ti})
	}
	return s.focusField(0)
}

func (s setupForm) focusField(i int) setupForm {
	if len(s.fields) == 0 {
		return s
	}
	i = (i + len(s.fields)) % len(s.fields)
	for j := range s.fields {
		if j == i {
			s.fields[j].ti.Focus()
		} else {
			s.fields[j].ti.Blur()
		}
	}
	s.focus = i
	return s
}

// changes are the --set arguments for edited vars and the secrets entered.
func (s setupForm) changes() (sets []string, secrets map[string]string) {
	for i := range s.fields {
		f := &s.fields[i]
		v := f.ti.Value()
		switch {
		case f.secret && strings.TrimSpace(v) != "":
			if secrets == nil {
				secrets = map[string]string{}
			}
			secrets[f.name] = v
		case !f.secret && v != f.orig:
			sets = append(sets, f.name+"="+v)
		}
	}
	return sets, secrets
}
