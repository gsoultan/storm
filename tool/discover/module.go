package tooldiscover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Module is the module being generated into — the adopter's, never storm's.
type Module struct {
	// Root is the absolute directory holding go.mod.
	Root string
	// Path is the module directive, which every synthesized import must spell.
	Path string
}

// FindModule walks up from start until it finds a go.mod.
//
// The tool is run from wherever the developer happens to be standing, which is
// usually the module root but is just as often a subdirectory. Requiring the
// root would be a rule to remember for no reason: the answer is discoverable,
// so discover it.
func FindModule(start string) (*Module, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(gomod); err == nil {
			path, err := modulePath(gomod)
			if err != nil {
				return nil, err
			}
			return &Module{Root: dir, Path: path}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("no go.mod at or above %s — storm generates into a module and needs its path to write imports", start)
		}
		dir = parent
	}
}

// ImportPath spells the import for a directory inside the module.
func (m *Module) ImportPath(dir string) (string, error) {
	rel, err := filepath.Rel(m.Root, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return m.Path, nil
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is outside module %s", dir, m.Path)
	}
	return m.Path + "/" + filepath.ToSlash(rel), nil
}

// modulePath reads the module directive. It is deliberately not a go.mod
// parser: the one line that matters is unambiguous, and depending on
// golang.org/x/mod to read it would put a dependency in the tool for a
// substring.
func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if rest == "" || rest == "(" {
			continue
		}
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		return strings.Trim(rest, "\"`"), nil
	}
	return "", fmt.Errorf("no module directive in %s", gomod)
}
