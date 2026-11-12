package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceLinks(t *testing.T) {
	// The working directory itself needs URL escaping even for simple relative
	// filenames. It is deliberately separate from any run/log directory.
	cwd := filepath.Join(t.TempDir(), "project space#percent%")
	if err := os.MkdirAll(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	for _, name := range []string{"src/main.py", "src/other.go", "src/a#b%.py"} {
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	host, _ := os.Hostname()
	link := func(path, text, fragment string) string {
		u := url.URL{Scheme: "file", Host: host, Path: filepath.Join(cwd, path), Fragment: fragment}
		return "\x1b]8;;" + u.String() + "\x1b\\" + text + "\x1b]8;;\x1b\\"
	}
	for _, tc := range []struct {
		name, input, want string
	}{
		{"flake8", "src/main.py:54:21: WPS237 message", link("src/main.py", "src/main.py:54:21", "L54C21") + ": WPS237 message"},
		{"module diagnostic", "src/main.py:0:1: WPS201 message", link("src/main.py", "src/main.py:0:1", "L0C1") + ": WPS201 message"},
		{"line only", "src/main.py:54: message", link("src/main.py", "src/main.py:54", "L54") + ": message"},
		{"indented", "  src/main.py:54:21 - error", "  " + link("src/main.py", "src/main.py:54:21", "L54C21") + " - error"},
		{"multiple", "src/main.py:1 and src/other.go:2:3\n", link("src/main.py", "src/main.py:1", "L1") + " and " + link("src/other.go", "src/other.go:2:3", "L2C3") + "\n"},
		{"quoted", `"src/main.py:54:21"`, `"` + link("src/main.py", "src/main.py:54:21", "L54C21") + `"`},
		{"colored", "\x1b[31msrc/main.py:54:21\x1b[0m: error", "\x1b[31m" + link("src/main.py", "src/main.py:54:21", "L54C21") + "\x1b[0m: error"},
		{"escaped filename", "src/a#b%.py:2", link("src/a#b%.py", "src/a#b%.py:2", "L2")},
		{"missing", "missing.py:1:2", "missing.py:1:2"},
		{"directory", "src:1:2", "src:1:2"},
		{"timestamp", "12:34:56 error", "12:34:56 error"},
		{"URL", "https://example.com:1234", "https://example.com:1234"},
		{"invalid position", "src/main.py:12abc", "src/main.py:12abc"},
		{"existing hyperlink", link("src/main.py", "src/main.py:54:21", "L54C21"), link("src/main.py", "src/main.py:54:21", "L54C21")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (outputOptions{links: true}).sourceLinks(tc.input); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
			if got := (outputOptions{}).sourceLinks(tc.input); got != tc.input {
				t.Errorf("links disabled: got %q; want %q", got, tc.input)
			}
		})
	}
}

func TestLogTailSourceLinks(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile("main.py", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	text := filepath.Join(cwd, "main.py") + ":54:21: error\nmain.py:0:1: error\n"
	path := filepath.Join(dir, "lint.log")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	printLogTail(&out, dir, Result{Name: "lint", Log: "lint.log", Status: StatusFailed, ExitCode: 1}, 15, outputOptions{links: true})
	for _, fragment := range []string{"#L54C21\x1b\\", "#L0C1\x1b\\"} {
		if !strings.Contains(out.String(), fragment) {
			t.Errorf("missing source link %q in %q", fragment, out.String())
		}
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != text {
		t.Fatalf("log contents = %q, %v; want %q", got, err, text)
	}
}
