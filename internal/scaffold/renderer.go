// Package scaffold renders service boilerplate from embedded templates.
package scaffold

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/*
var templateFS embed.FS

// ServiceData is the template context used for all three scaffold files.
type ServiceData struct {
	Name      string
	Container string
	Port      string
}

// File holds a rendered file ready to be written to disk.
type File struct {
	RelPath string // e.g. "services/paperless/docker-compose.yml"
	Content string
}

// Render executes all three templates with data and returns the results.
func Render(data ServiceData) ([]File, error) {
	type entry struct {
		tmpl    string
		relPath string
	}
	entries := []entry{
		{"docker-compose.yml.tmpl", fmt.Sprintf("services/%s/docker-compose.yml", data.Name)},
		{"caddy.conf.tmpl", fmt.Sprintf("services/%s/caddy.conf", data.Name)},
		{"caddy.cf.conf.tmpl", fmt.Sprintf("services/%s/caddy.cf.conf", data.Name)},
		{"config.yaml.tmpl", fmt.Sprintf("services/%s/config.yaml", data.Name)},
	}

	out := make([]File, len(entries))
	for i, e := range entries {
		content, err := exec(e.tmpl, data)
		if err != nil {
			return nil, fmt.Errorf("rendering %s: %w", e.tmpl, err)
		}
		out[i] = File{RelPath: e.relPath, Content: content}
	}
	return out, nil
}

// Write creates the service directory and writes all rendered files to disk.
// It errors if the service directory already exists.
func Write(repoRoot string, files []File) error {
	if len(files) == 0 {
		return nil
	}
	// The first file's parent dir is services/<name>/ — check it doesn't exist.
	svcDir := filepath.Dir(filepath.Join(repoRoot, files[0].RelPath))
	if _, err := os.Stat(svcDir); err == nil {
		return fmt.Errorf("directory already exists: %s", svcDir)
	}
	for _, f := range files {
		path := filepath.Join(repoRoot, f.RelPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(f.Content), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func exec(name string, data ServiceData) (string, error) {
	raw, err := templateFS.ReadFile("templates/" + name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
