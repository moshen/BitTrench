package service

import "testing"

// The registered value is a command line rather than a path, so finding a stray
// installation of this binary means parsing the executable back out of one. The
// quoting is where this would go wrong.
func TestCommandLineRefersTo(t *testing.T) {
	const exe = `C:\Program Files\bittrench\bittrench.exe`

	tests := []struct {
		name        string
		commandLine string
		exe         string
		want        bool
	}{
		{
			name:        "quoted path with arguments",
			commandLine: `"C:\Program Files\bittrench\bittrench.exe" serve -config "C:\ProgramData\bittrench\config.toml"`,
			exe:         exe,
			want:        true,
		},
		{
			name:        "unquoted path with arguments",
			commandLine: `C:\bittrench.exe serve -config C:\config.toml`,
			exe:         `C:\bittrench.exe`,
			want:        true,
		},
		{
			name:        "bare path, no arguments",
			commandLine: exe,
			exe:         exe,
			want:        true,
		},
		{
			// Windows paths are case-insensitive, and the SCM hands back
			// whatever case was registered.
			name:        "case-insensitive",
			commandLine: `"c:\program files\bittrench\BITTRENCH.EXE" serve`,
			exe:         exe,
			want:        true,
		},
		{
			name:        "a different binary in the same directory",
			commandLine: `"C:\Program Files\bittrench\other.exe" serve`,
			exe:         exe,
			want:        false,
		},
		{
			// The point of comparing only the first token: an unrelated service
			// that merely mentions our path must not count.
			name:        "our path appearing only as an argument",
			commandLine: `"C:\Windows\System32\cmd.exe" /c "C:\Program Files\bittrench\bittrench.exe"`,
			exe:         exe,
			want:        false,
		},
		{
			// Separators are interchangeable on Windows, and the SCM stores
			// whatever was registered.
			name:        "forward slashes",
			commandLine: `"C:/Program Files/bittrench/bittrench.exe" serve`,
			exe:         exe,
			want:        true,
		},
		{
			name:        "an unterminated quote is still compared",
			commandLine: `"C:\Program Files\bittrench\bittrench.exe`,
			exe:         exe,
			want:        true,
		},
		{name: "empty command line", commandLine: "", exe: exe, want: false},
		{name: "empty exe", commandLine: exe, exe: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandLineRefersTo(tc.commandLine, tc.exe); got != tc.want {
				t.Errorf("commandLineRefersTo(%q, %q) = %v, want %v",
					tc.commandLine, tc.exe, got, tc.want)
			}
		})
	}
}
