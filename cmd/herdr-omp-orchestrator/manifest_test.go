package main

// The Windows manifest is the root herdr-plugin.toml. The Linux manifest is
// linux/herdr-plugin.toml, which install.sh copies into build/linux-plugin/.
// These tests pin that the two declare the same plugin, differing only where
// the platform requires it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifestDoc is a parsed manifest: the keys before the first `[[…]]` section
// and every section in file order. Values keep their raw text, quotes included.
type manifestDoc struct {
	top      map[string]string
	sections []manifestSection
}

// readManifestDoc is a small reader for a plugin manifest. Blank and `#` lines
// are skipped, `[[kind]]` opens a section, and `key = value` goes to top before
// the first section and to the current section after it.
func readManifestDoc(t *testing.T, path string) manifestDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the manifest %s: %v", path, err)
	}
	doc := manifestDoc{top: map[string]string{}}
	current := -1
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]"):
			doc.sections = append(doc.sections, manifestSection{kind: strings.Trim(line, "[]"), fields: map[string]string{}})
			current = len(doc.sections) - 1
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if current < 0 {
				doc.top[key] = value
			} else {
				doc.sections[current].fields[key] = value
			}
		}
	}
	return doc
}

// nonBuildSections drops the `[[build]]` sections, which only the Windows
// manifest declares.
func nonBuildSections(sections []manifestSection) []manifestSection {
	var out []manifestSection
	for _, section := range sections {
		if section.kind != "build" {
			out = append(out, section)
		}
	}
	return out
}

func TestLinuxManifestMirrorsTheWindowsManifest(t *testing.T) {
	root := readManifestDoc(t, filepath.Join("..", "..", "herdr-plugin.toml"))
	linux := readManifestDoc(t, filepath.Join("..", "..", "linux", "herdr-plugin.toml"))

	for _, key := range []string{"id", "name", "version", "min_herdr_version"} {
		if root.top[key] != linux.top[key] {
			t.Errorf("%s: windows %s, linux %s", key, root.top[key], linux.top[key])
		}
	}
	if got := root.top["platforms"]; got != `["windows"]` {
		t.Errorf("root platforms = %s, want [\"windows\"]", got)
	}
	if got := linux.top["platforms"]; got != `["linux"]` {
		t.Errorf("linux platforms = %s, want [\"linux\"]", got)
	}

	buildSections := 0
	for _, section := range root.sections {
		if section.kind == "build" {
			buildSections++
		}
	}
	if buildSections != 1 {
		t.Errorf("root has %d build sections, want 1", buildSections)
	}
	for _, section := range linux.sections {
		if section.kind == "build" {
			t.Errorf("linux manifest declares a build section")
		}
	}

	windows := nonBuildSections(root.sections)
	if len(windows) != len(linux.sections) {
		t.Fatalf("windows has %d sections besides build, linux has %d", len(windows), len(linux.sections))
	}
	for i, want := range windows {
		got := linux.sections[i]
		if got.kind != want.kind {
			t.Fatalf("section %d: windows [[%s]], linux [[%s]]", i, want.kind, got.kind)
		}
		if len(got.fields) != len(want.fields) {
			t.Fatalf("section %d [[%s]]: windows fields %v, linux fields %v", i, want.kind, want.fields, got.fields)
		}
		for key, value := range want.fields {
			linuxValue, ok := got.fields[key]
			if !ok {
				t.Fatalf("section %d [[%s]]: linux has no %s", i, want.kind, key)
			}
			if key == "command" {
				if strings.ReplaceAll(value, ".exe", "") != linuxValue {
					t.Errorf("section %d [[%s]] command: windows %s, linux %s", i, want.kind, value, linuxValue)
				}
				continue
			}
			if value != linuxValue {
				t.Errorf("section %d [[%s]] %s: windows %s, linux %s", i, want.kind, key, value, linuxValue)
			}
		}
	}
}

func TestLinuxManifestCommandsNameTheLinuxBinary(t *testing.T) {
	linux := readManifestDoc(t, filepath.Join("..", "..", "linux", "herdr-plugin.toml"))
	commands := 0
	for _, section := range linux.sections {
		command, ok := section.fields["command"]
		if !ok {
			continue
		}
		commands++
		if !strings.HasPrefix(command, `["./herdr-omp-orchestrator"`) {
			t.Errorf("[[%s]] command %s does not start with ./herdr-omp-orchestrator", section.kind, command)
		}
		if strings.Contains(command, ".exe") {
			t.Errorf("[[%s]] command %s names a .exe", section.kind, command)
		}
	}
	if commands == 0 {
		t.Fatal("linux manifest declares no commands")
	}
}

func TestRootManifestStaysWindowsOnly(t *testing.T) {
	root := readManifestDoc(t, filepath.Join("..", "..", "herdr-plugin.toml"))
	if got := root.top["platforms"]; got != `["windows"]` {
		t.Errorf("root platforms = %s, want [\"windows\"]", got)
	}
	for _, section := range nonBuildSections(root.sections) {
		command, ok := section.fields["command"]
		if !ok {
			continue
		}
		if !strings.HasPrefix(command, `["./herdr-omp-orchestrator.exe"`) {
			t.Errorf("[[%s]] command %s does not name ./herdr-omp-orchestrator.exe", section.kind, command)
		}
	}
}
