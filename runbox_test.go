package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	good := map[string]Command{
		"build=go build ./...": {"build", "go build ./..."},
		"a.b-c_1=x=y":          {"a.b-c_1", "x=y"}, // only the first '=' splits
	}
	for in, want := range good {
		got, err := parseCommand(in)
		if err != nil || got != want {
			t.Errorf("parseCommand(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "noequals", "=cmd", "a/b=ls", "../x=ls", ".hidden=ls", "-x=ls", "a b=ls", "name=", "name=   "} {
		if _, err := parseCommand(in); err == nil {
			t.Errorf("parseCommand(%q) succeeded; want error", in)
		}
	}
}

func TestParseCommandsDuplicatesAndEmpty(t *testing.T) {
	if _, err := parseCommands([]string{"a=true", "a=false"}); err == nil {
		t.Error("duplicate names accepted")
	}
	if _, err := parseCommands(nil); err == nil {
		t.Error("no commands accepted")
	}
}

func TestCreateRunDirNumbering(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".runbox")
	for want := 1; want <= 3; want++ {
		_, n, err := createRunDir(base)
		if err != nil || n != want {
			t.Fatalf("got n=%d err=%v; want %d", n, err, want)
		}
	}
}

func TestCreateRunDirConcurrent(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".runbox")
	const workers = 20
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, n, err := createRunDir(base)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[n] {
				t.Errorf("run number %d handed out twice", n)
			}
			seen[n] = true
		}()
	}
	wg.Wait()
	if len(seen) != workers {
		t.Errorf("got %d distinct runs; want %d", len(seen), workers)
	}
}

func TestUpdateLatest(t *testing.T) {
	base := t.TempDir()
	for _, n := range []int{1, 2} {
		if err := updateLatest(base, n); err != nil {
			t.Fatal(err)
		}
		got, err := os.Readlink(filepath.Join(base, "latest"))
		if err != nil || got != fmt.Sprintf("run-%d", n) {
			t.Errorf("latest -> %q, %v; want run-%d", got, err, n)
		}
	}
}

func TestPrune(t *testing.T) {
	base := t.TempDir()
	for i := 1; i <= 15; i++ {
		os.Mkdir(filepath.Join(base, fmt.Sprintf("run-%d", i)), 0o755)
	}
	os.Mkdir(filepath.Join(base, "unrelated"), 0o755)
	os.WriteFile(filepath.Join(base, "run-1x"), nil, 0o644)

	if err := prune(base, 10, 15); err != nil {
		t.Fatal(err)
	}
	nums, _ := listRuns(base)
	if len(nums) != 10 || nums[0] != 6 || nums[9] != 15 {
		t.Errorf("remaining runs = %v; want 6..15", nums)
	}
	for _, name := range []string{"unrelated", "run-1x"} {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
}

func TestRunAll(t *testing.T) {
	dir := t.TempDir()
	results := runAll(context.Background(), nil, dir, "sh", 0, []Command{
		{"ok", "echo out; echo err >&2"},
		{"fail", "echo boom; exit 3"},
		{"missing", "definitely-not-a-command-xyz"},
		{"sig", "kill -TERM $$"},
	}, nil)
	want := []struct {
		status string
		code   int
	}{{StatusOK, 0}, {StatusFailed, 3}, {StatusFailed, 127}, {StatusSignaled, -1}}
	for i, w := range want {
		if results[i].Status != w.status || results[i].ExitCode != w.code {
			t.Errorf("%s: got %s/%d; want %s/%d", results[i].Name, results[i].Status, results[i].ExitCode, w.status, w.code)
		}
	}
	if results[3].Signal != "TERM" {
		t.Errorf("signal = %q; want TERM", results[3].Signal)
	}
	// stdout and stderr both land in the log.
	data, _ := os.ReadFile(filepath.Join(dir, "ok.log"))
	if s := string(data); s != "out\nerr\n" {
		t.Errorf("ok.log = %q", s)
	}
}

func TestRunAllIsConcurrent(t *testing.T) {
	start := time.Now()
	runAll(context.Background(), nil, t.TempDir(), "sh", 0, []Command{{"a", "sleep 1"}, {"b", "sleep 1"}, {"c", "sleep 1"}}, nil)
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Errorf("took %v; commands do not look concurrent", d)
	}
}

func TestRunAllCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	// The nested sleep checks that children of the shell are terminated too.
	results := runAll(ctx, nil, t.TempDir(), "sh", 0, []Command{{"slow", "sleep 30; echo unreachable"}, {"quick", "true"}}, nil)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if results[0].Status != StatusCanceled {
		t.Errorf("slow status = %s; want canceled", results[0].Status)
	}
	if results[1].Status != StatusOK {
		t.Errorf("quick status = %s; want ok", results[1].Status)
	}
}

func TestWriteResults(t *testing.T) {
	dir := t.TempDir()
	in := []Result{{Name: "a", Command: "true", Status: StatusOK, Log: "a.log"}}
	if err := writeResults(dir, in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "results.json"))
	var out []Result
	if err := json.Unmarshal(data, &out); err != nil || len(out) != 1 || out[0] != in[0] {
		t.Errorf("round trip = %v, %v", out, err)
	}
}

func TestRunEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := run([]string{"--run", "a=true", "--run", "b=exit 1"}); code != 1 {
		t.Errorf("exit code = %d; want 1", code)
	}
	if code := run([]string{"--run", "a=true"}); code != 0 {
		t.Errorf("exit code = %d; want 0", code)
	}
	if got, _ := os.Readlink(".runbox/latest"); got != "run-2" {
		t.Errorf("latest -> %q; want run-2", got)
	}
	if _, err := os.Stat(".runbox/run-1/results.json"); err != nil {
		t.Error(err)
	}
	if code := run([]string{"--run", "bad"}); code != 2 {
		t.Errorf("exit code = %d; want 2", code)
	}
}

func TestPrintReportTailOptions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.log"), []byte("a\nb\nc\n"), 0o644)
	results := []Result{{Name: "bad", Status: StatusFailed, ExitCode: 1, Log: "bad.log"}}
	var out strings.Builder
	printReport(&out, dir, results, 2, 0, outputOptions{paths: pathsAbs})
	if s := out.String(); !strings.Contains(s, "b\nc\n") || strings.Contains(s, "a\n") {
		t.Errorf("tail=2 output wrong:\n%s", s)
	}
	out.Reset()
	printReport(&out, dir, results, 0, 0, outputOptions{tty: true, paths: pathsBase})
	if s := out.String(); strings.Contains(s, "---") || !strings.Contains(s, "bad: FAILED exit=1") {
		t.Errorf("tail=0 output wrong:\n%s", s)
	}
}

func TestPrintReport(t *testing.T) {
	dir := t.TempDir()
	var long strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	os.WriteFile(filepath.Join(dir, "good.log"), []byte("fine\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.log"), []byte(long.String()), 0o644)
	os.WriteFile(filepath.Join(dir, "empty.log"), nil, 0o644)
	results := []Result{
		{Name: "good", Status: StatusOK, Log: "good.log"},
		{Name: "bad", Status: StatusFailed, ExitCode: 2, Log: "bad.log"},
		{Name: "empty", Status: StatusCanceled, ExitCode: -1, Log: "empty.log"},
	}
	var out strings.Builder
	if !printReport(&out, dir, results, 20, 2500*time.Millisecond, outputOptions{paths: pathsAbs}) {
		t.Error("expected failure to be reported")
	}
	s := out.String()
	for _, want := range []string{
		"bad: FAILED exit=2 (last 20 lines of bad.log)",
		"empty: CANCELED (empty.log)", "in 2.5s",
		"line 50", "line 31", "(log is empty)", "summary: 1/3 ok, 1 failed, 1 canceled",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "last 20 lines of empty.log") {
		t.Errorf("header claims 20 lines for an empty log:\n%s", s)
	}
	if strings.Contains(s, "good: OK") || strings.Contains(s, "line 30\n") || strings.Contains(s, "fine\n") {
		t.Errorf("report has too much log output:\n%s", s)
	}
}

func TestRunAllForceKill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	force := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, cancel)
	time.AfterFunc(400*time.Millisecond, func() { close(force) })
	start := time.Now()
	results := runAll(ctx, force, t.TempDir(), "sh", 0, []Command{{"stubborn", "trap '' TERM; sleep 30"}}, nil)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("force kill took %v", d)
	}
	if results[0].Status != StatusCanceled {
		t.Errorf("status = %s; want canceled", results[0].Status)
	}
}

func TestTailByteLimit(t *testing.T) {
	if got := tailByteLimit(9_000_000_000_000_000_000); got != maxTailBytes {
		t.Errorf("huge tail limit = %d", got)
	}
	if got := tailByteLimit(15); got != 15*tailBytesPerLine {
		t.Errorf("limit = %d", got)
	}
}

func TestRunRejectsPositionalArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := run([]string{"--run", "x=true", "y=echo"}); code != 2 {
		t.Errorf("exit code = %d; want 2", code)
	}
	if _, err := os.Stat(baseDir); err == nil {
		t.Error("run directory created despite usage error")
	}
}

func TestPrintReportTableAndClipping(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "big.log"), []byte(strings.Repeat("x", 200_000)), 0o644)
	results := []Result{
		{Name: "a", Status: StatusOK, Log: "a.log", DurationMS: 2502},
		{Name: "big", Status: StatusSignaled, Signal: "TERM", ExitCode: -1, Log: "big.log"},
	}
	var out strings.Builder
	printReport(&out, dir, results, 15, 3*time.Second, outputOptions{tty: true, paths: pathsBase})
	s := out.String()
	for _, want := range []string{"a:   OK", "2.5s", "SIGNALED (TERM)", "a.log", "may start mid-line", "195 KB", "in 3.0s"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, dir) || strings.Contains(s, "exit=-1") {
		t.Errorf("base paths / signal wording violated:\n%s", s)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		250 * time.Millisecond:  "250ms",
		2502 * time.Millisecond: "2.5s",
		83 * time.Second:        "1m23s",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q; want %q", d, got, want)
		}
	}
}

func TestProgressNonTTYStreamsFinishedCommands(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, outputOptions{paths: pathsAbs}, "/d", 2)
	p.started("a")
	p.finished(Result{Name: "a", Status: StatusFailed, ExitCode: 4, Log: "a.log", DurationMS: 1200})
	p.stop()
	if got, want := out.String(), "a: FAILED exit=4 1.2s /d/a.log\n"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestVersionFlag(t *testing.T) {
	if code := run([]string{"--version"}); code != 0 {
		t.Errorf("exit code = %d; want 0", code)
	}
	if versionString() == "" {
		t.Error("empty version")
	}
}

func TestHelpAndBadFlag(t *testing.T) {
	if code := run([]string{"--help"}); code != 0 {
		t.Errorf("--help exit code = %d; want 0", code)
	}
	if code := run([]string{"--bogus"}); code != 2 {
		t.Errorf("bad flag exit code = %d; want 2", code)
	}
}

func TestStatusLineFitsWidth(t *testing.T) {
	p := newProgress(io.Discard, outputOptions{tty: true}, "/d", 6)
	p.stop()
	names := []string{"alpha(5s)", "bravo(5s)", "charlie(5s)", "delta(5s)", "echo(5s)", "slow(5s)"}
	for _, w := range []int{20, 40, 60, 80, 200} {
		p.width = func() int { return w }
		line := p.statusLine("runbox: 0/6 done", names)
		if n := len([]rune(line)); n >= w {
			t.Errorf("width %d: line has %d columns: %q", w, n, line)
		}
	}
	p.width = func() int { return 60 }
	if got, want := p.statusLine("runbox: 0/6 done", names), "runbox: 0/6 done — running: alpha(5s), bravo(5s), +4 more"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestStatusLineCanceling(t *testing.T) {
	p := newProgress(io.Discard, outputOptions{tty: true}, "/d", 1)
	p.stop()
	p.width = func() int { return 100 }
	p.state = canceling
	if got := p.statusLine("x", []string{"a(1s)"}); !strings.Contains(got, "canceling… (Ctrl-C again to force)") || !strings.HasSuffix(got, "running: a(1s)") {
		t.Errorf("got %q", got)
	}
}

func TestTermWidthFallback(t *testing.T) {
	t.Setenv("COLUMNS", "")
	if got := termWidth(nil); got != defaultTermWidth {
		t.Errorf("got %d", got)
	}
	t.Setenv("COLUMNS", "55")
	if got := termWidth(nil); got != 55 {
		t.Errorf("got %d", got)
	}
}

func TestQuiet(t *testing.T) {
	t.Chdir(t.TempDir())
	out := captureStdout(t, func() {
		if code := run([]string{"-q", "--run", "a=true"}); code != 0 {
			t.Errorf("exit code = %d; want 0", code)
		}
	})
	if out != "" {
		t.Errorf("quiet success printed %q", out)
	}
	out = captureStdout(t, func() {
		if code := run([]string{"--quiet", "--run", "a=true", "--run", "b=echo boom; exit 3"}); code != 1 {
			t.Errorf("exit code = %d; want 1", code)
		}
	})
	if strings.Contains(out, "runbox: running") || strings.Contains(out, "a:") {
		t.Errorf("quiet failure printed header or success lines: %q", out)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "summary: 1/2 ok, 1 failed") {
		t.Errorf("quiet failure missing tail or summary: %q", out)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestStatusLinePaintsFailedCounter(t *testing.T) {
	p := newProgress(io.Discard, outputOptions{tty: true, color: true}, "/d", 3)
	p.stop()
	p.width = func() int { return 80 }
	p.done, p.failed = 2, 1
	want := "runbox: 2/3 done, \x1b[31m1 failed\x1b[0m — running: a(1s)"
	if got := p.statusLine("runbox: 2/3 done, 1 failed", []string{"a(1s)"}); got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestTailHeaderOnlyClaimsCountWhenCut(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "short.log"), []byte("a\nb\nc\n"), 0o644)
	var out strings.Builder
	printReport(&out, dir, []Result{{Name: "s", Status: StatusFailed, ExitCode: 1, Log: "short.log"}}, 15, time.Second, outputOptions{paths: pathsAbs})
	if s := out.String(); !strings.Contains(s, "s: FAILED exit=1 (short.log)") || strings.Contains(s, "last 15") {
		t.Errorf("unexpected header:\n%s", s)
	}
}

func TestStatusLineRunningOrderLongestFirst(t *testing.T) {
	p := newProgress(io.Discard, outputOptions{tty: true}, "/d", 2)
	p.stop()
	now := time.Now()
	p.running["a"] = now
	p.running["z"] = now.Add(-10 * time.Second)
	var out strings.Builder
	p.w = &out
	p.width = func() int { return 200 }
	p.draw()
	if s := out.String(); strings.Index(s, "z(") > strings.Index(s, "a(") {
		t.Errorf("longest-running not first: %q", s)
	}
}

func TestLinks(t *testing.T) {
	on := outputOptions{links: true}
	got := on.link("/tmp/a b/x.log", "x.log")
	if !strings.HasPrefix(got, "\x1b]8;;file://") || !strings.Contains(got, "/tmp/a%20b/x.log\x1b\\x.log\x1b]8;;\x1b\\") {
		t.Errorf("link = %q", got)
	}
	if got := (outputOptions{}).link("/tmp/x.log", "x.log"); got != "x.log" {
		t.Errorf("links off: %q", got)
	}
	if got := on.link("rel/x.log", "x.log"); got != "x.log" {
		t.Errorf("relative path was linked: %q", got)
	}
}

func TestPipedOutputHasNoEscapes(t *testing.T) {
	t.Chdir(t.TempDir())
	out := captureStdout(t, func() { run([]string{"--run", "a=true", "--run", "b=exit 1"}) })
	if strings.Contains(out, "\x1b") {
		t.Errorf("piped output has escapes: %q", out)
	}
}

func TestCheckShell(t *testing.T) {
	if err := checkShell("sh"); err != nil {
		t.Errorf("default shell rejected: %v", err)
	}
	if err := checkShell("/nonexistent/shell"); err == nil {
		t.Error("missing shell accepted")
	}
	if err := checkShell(t.TempDir()); err == nil {
		t.Error("directory accepted as shell")
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkShell(plain); err == nil {
		t.Error("non-executable file accepted as shell")
	}
}

func TestRunAllCustomShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	// [[ ]] is a bashism that dash-style sh may reject.
	results := runAll(context.Background(), nil, t.TempDir(), bash, 0, []Command{{"a", "[[ a == a ]]"}}, nil)
	if results[0].Status != StatusOK || results[0].Shell != bash {
		t.Errorf("got %+v", results[0])
	}
}

func TestRunAllTimeout(t *testing.T) {
	old := killGrace
	killGrace = 300 * time.Millisecond
	defer func() { killGrace = old }()

	start := time.Now()
	results := runAll(context.Background(), nil, t.TempDir(), "sh", 200*time.Millisecond, []Command{
		{"slow", "sleep 30"},
		{"stubborn", "trap '' TERM; sleep 30"},
		{"quick", "true"},
	}, nil)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeout took %v", d)
	}
	for i, want := range []string{StatusTimedOut, StatusTimedOut, StatusOK} {
		if results[i].Status != want {
			t.Errorf("%s: status = %s; want %s", results[i].Name, results[i].Status, want)
		}
	}
}

func TestTimeoutDoesNotOverrideCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	results := runAll(ctx, nil, t.TempDir(), "sh", time.Minute, []Command{{"slow", "sleep 30"}}, nil)
	if results[0].Status != StatusCanceled {
		t.Errorf("status = %s; want canceled", results[0].Status)
	}
}
