package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The search order is a compatibility surface of its own: an operator with a
// config in the working directory must keep getting it, and a per-user file must
// beat the machine-wide one rather than the reverse.
func TestSearchPaths(t *testing.T) {
	envOf := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	home := func(dir string) func() (string, error) {
		return func() (string, error) { return dir, nil }
	}
	// searchPaths parameterises which entries appear, not how they are spelled:
	// the spelling comes from the host's filepath either way. So every want is
	// built with filepath.Join, and the one value that has to satisfy
	// filepath.IsAbs is spelled for the host - "/xdg" is not absolute on
	// Windows, which would send that case down the home-directory branch.
	absDir := func(name string) string {
		if runtime.GOOS == "windows" {
			return filepath.Join(`C:\`, name)
		}
		return "/" + name
	}
	opHome := absDir("home/op")
	xdgHome := absDir("xdg")

	tests := []struct {
		name string
		goos string
		env  map[string]string
		home func() (string, error)
		want []string
	}{
		{
			name: "linux falls back to ~/.config",
			goos: "linux",
			home: home(opHome),
			want: []string{FileName, filepath.Join(opHome, ".config", appDir, FileName)},
		},
		{
			name: "XDG_CONFIG_HOME overrides the fallback",
			goos: "linux",
			env:  map[string]string{"XDG_CONFIG_HOME": xdgHome},
			home: home(opHome),
			want: []string{FileName, filepath.Join(xdgHome, appDir, FileName)},
		},
		{
			// XDG says a relative value is invalid. It would otherwise resolve
			// against the working directory and shadow the entry above it.
			name: "a relative XDG_CONFIG_HOME is ignored",
			goos: "linux",
			env:  map[string]string{"XDG_CONFIG_HOME": filepath.Join("relative", "dir")},
			home: home(opHome),
			want: []string{FileName, filepath.Join(opHome, ".config", appDir, FileName)},
		},
		{
			name: "windows gets the XDG location too, then ProgramData",
			goos: "windows",
			env:  map[string]string{"ProgramData": `C:\ProgramData`},
			home: home(`C:\Users\op`),
			want: []string{
				"config.toml",
				filepath.Join(`C:\Users\op`, ".config", "bittrench", "config.toml"),
				filepath.Join(`C:\ProgramData`, "bittrench", "config.toml"),
			},
		},
		{
			name: "ProgramData falls back to its conventional path",
			goos: "windows",
			home: home(`C:\Users\op`),
			want: []string{
				"config.toml",
				filepath.Join(`C:\Users\op`, ".config", "bittrench", "config.toml"),
				filepath.Join(`C:\ProgramData`, "bittrench", "config.toml"),
			},
		},
		{
			// No home directory is not a reason to report nothing at all.
			name: "an unknown home directory drops only that entry",
			goos: "linux",
			home: func() (string, error) { return "", errors.New("no home") },
			want: []string{"config.toml"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := searchPaths(tc.goos, envOf(tc.env), tc.home)
			if len(got) != len(tc.want) {
				t.Fatalf("paths = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("paths = %v, want %v", got, tc.want)
					break
				}
			}
		})
	}
}

func TestDiscoverTakesTheFirstThatExists(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.toml")
	second := filepath.Join(dir, "second.toml")
	if err := os.WriteFile(second, []byte("# second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discover([]string{first, second})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != second {
		t.Errorf("discover picked %q, want %q", got, second)
	}

	// Once the earlier candidate exists it wins, which is what makes the order
	// mean anything.
	if err := os.WriteFile(first, []byte("# first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = discover([]string{first, second}); got != first {
		t.Errorf("discover picked %q, want the earlier %q", got, first)
	}
}

// A directory in a candidate position is not a config file.
func TestDiscoverIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.toml")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real.toml")
	if err := os.WriteFile(real, []byte("# real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := discover([]string{asDir, real})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != real {
		t.Errorf("discover picked %q, want %q", got, real)
	}
}

// "No configuration file found" on its own leaves an operator guessing at
// exactly the moment they need telling.
func TestDiscoverNamesEverywhereItLooked(t *testing.T) {
	dir := t.TempDir()
	candidates := []string{
		filepath.Join(dir, "a.toml"),
		filepath.Join(dir, "b.toml"),
	}
	_, err := discover(candidates)
	if err == nil {
		t.Fatal("expected an error when nothing exists")
	}
	for _, c := range candidates {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("error %q does not name %q", err, c)
		}
	}
}

// The real entry point has to agree with the tested core.
func TestSearchPathsIsNotEmpty(t *testing.T) {
	paths := SearchPaths()
	if len(paths) == 0 || paths[0] != FileName {
		t.Errorf("SearchPaths() = %v, want the working directory first", paths)
	}
}
