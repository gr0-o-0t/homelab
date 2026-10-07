package gui

import (
	"encoding/json"
	"strings"
)

// The Config tab edits what `homelab setup [svc] --json` reports and saves it
// with `setup [svc] --set=K=V … --secrets-stdin`. Secret values travel only on
// stdin, never on argv, and are never read back: the JSON says set/unset.

// setupJSON mirrors cmd.SetupInfo, the `setup --json` document.
type setupJSON struct {
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

type varField struct {
	Name, Description string
	Required          bool
	Orig, Value       string
}

type secretField struct {
	Name, Description  string
	Required, Set, Gen bool
	Value              string // typed by the user; "" keeps what is stored
	Reveal             bool   // UI: show the typed value
}

// setupForm is the editable model of one setup document.
type setupForm struct {
	Service string // "" for the root config
	Vars    []varField
	Secrets []secretField
}

// parseSetup reads `setup --json` output. Anything before the JSON object
// (a banner) is skipped.
func parseSetup(out []byte) (*setupForm, error) {
	if i := strings.IndexByte(string(out), '{'); i > 0 {
		out = out[i:]
	}
	var doc setupJSON
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	f := &setupForm{Service: doc.Service}
	for _, v := range doc.Vars {
		f.Vars = append(f.Vars, varField{Name: v.Name, Description: v.Description, Required: v.Required, Orig: v.Value, Value: v.Value})
	}
	for _, s := range doc.Secrets {
		f.Secrets = append(f.Secrets, secretField{Name: s.Name, Description: s.Description, Required: s.Required, Set: s.Set, Gen: s.Generated})
	}
	return f, nil
}

// secretStatus is the badge text of a secret field.
func (s secretField) status() string {
	switch {
	case strings.TrimSpace(s.Value) != "":
		return "will change"
	case s.Set:
		return "set"
	case s.Gen:
		return "generated"
	}
	return "unset"
}

// Dirty reports whether saving would change anything.
func (f *setupForm) Dirty() bool {
	for _, v := range f.Vars {
		if v.Value != v.Orig {
			return true
		}
	}
	for _, s := range f.Secrets {
		if strings.TrimSpace(s.Value) != "" {
			return true
		}
	}
	return false
}

// Missing lists required settings that would still be empty after saving.
func (f *setupForm) Missing() []string {
	var out []string
	for _, v := range f.Vars {
		if v.Required && strings.TrimSpace(v.Value) == "" {
			out = append(out, v.Name)
		}
	}
	for _, s := range f.Secrets {
		if s.Required && !s.Set && !s.Gen && strings.TrimSpace(s.Value) == "" {
			out = append(out, s.Name)
		}
	}
	return out
}

// Save returns the argv (after the binary) and the stdin that save the
// changes: changed vars as --set=K=V, typed secrets as a JSON object on stdin
// with --secrets-stdin. stdin is nil when no secret changes.
func (f *setupForm) Save() (args []string, stdin []byte) {
	args = []string{"setup"}
	if f.Service != "" {
		args = append(args, f.Service)
	}
	for _, v := range f.Vars {
		if v.Value != v.Orig {
			args = append(args, "--set="+v.Name+"="+v.Value)
		}
	}
	secrets := map[string]string{}
	for _, s := range f.Secrets {
		if v := strings.TrimSpace(s.Value); v != "" {
			secrets[s.Name] = v
		}
	}
	if len(secrets) > 0 {
		stdin, _ = json.Marshal(secrets)
		args = append(args, "--secrets-stdin")
	}
	return args, stdin
}

// loadArgs is the argv that reads the form.
func loadSetupArgs(svc string) []string {
	if svc == "" {
		return []string{"setup", "--json"}
	}
	return []string{"setup", svc, "--json"}
}

// clone is a deep copy, so the UI can edit while the loaded form stays intact.
func (f *setupForm) clone() *setupForm {
	if f == nil {
		return nil
	}
	c := *f
	c.Vars = append([]varField(nil), f.Vars...)
	c.Secrets = append([]secretField(nil), f.Secrets...)
	return &c
}
