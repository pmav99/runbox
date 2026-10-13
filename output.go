package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultTail = 15

	// tailBytesPerLine bounds how much of a log is read per requested line,
	// so a log with huge lines can't flood the output.
	tailBytesPerLine = 1024
	maxTailBytes     = 1 << 20

	pathsAbs  = "abs"
	pathsBase = "base"
)

// outputOptions selects between the two output styles: a human one for
// terminals (live status line, aligned table, colors, short paths) and a
// plain one for pipes, i.e. agents (one line per command as it finishes,
// absolute paths, no escape codes).
type outputOptions struct {
	tty   bool
	color bool
	links bool   // wrap paths in OSC 8 hyperlinks (terminals only)
	quiet bool   // print only failures: no header, progress, table or success summary
	paths string // pathsAbs or pathsBase; empty means the default for the style
}

func newOutputOptions(f *os.File, paths string) outputOptions {
	o := outputOptions{paths: paths}
	if st, err := f.Stat(); err == nil {
		o.tty = st.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
	}
	o.color = o.tty && os.Getenv("NO_COLOR") == ""
	o.links = o.color
	if o.paths == "" {
		o.paths = pathsAbs
		if o.tty {
			o.paths = pathsBase
		}
	}
	return o
}

// path renders a file in the run directory according to the paths policy.
func (o outputOptions) path(dir, name string) string {
	if o.paths == pathsBase {
		return name
	}
	return filepath.Join(dir, name)
}

// link renders text as a clickable file:// hyperlink to path (OSC 8) when
// links are enabled, and as plain text otherwise.
func (o outputOptions) link(path, text string) string {
	if !o.links || !filepath.IsAbs(path) {
		return text
	}
	host, _ := os.Hostname()
	u := url.URL{Scheme: "file", Host: host, Path: path}
	return "\x1b]8;;" + u.String() + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// pathLink is path, linked to the file's absolute location.
func (o outputOptions) pathLink(dir, name string) string {
	return o.link(filepath.Join(dir, name), o.path(dir, name))
}

func (o outputOptions) paint(code, s string) string {
	if !o.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// describe renders a result's status without a misleading "exit=-1": commands
// that never started, were canceled or died from a signal get their own wording.
func describe(r Result) string {
	switch {
	case r.Status == StatusOK:
		return "OK"
	case r.Status == StatusCanceled:
		return "CANCELED"
	case r.Status == StatusTimedOut:
		return "TIMED OUT"
	case r.Status == StatusSignaled:
		return "SIGNALED (" + r.Signal + ")"
	case r.Error != "":
		return "FAILED (could not start)"
	default:
		return fmt.Sprintf("FAILED exit=%d", r.ExitCode)
	}
}

func (o outputOptions) paintStatus(r Result, s string) string {
	switch r.Status {
	case StatusOK:
		return o.paint("32", s)
	case StatusCanceled, StatusSignaled:
		return o.paint("33", s)
	}
	return o.paint("31", s)
}

// formatDuration renders 2502ms as "2.5s" and 83s as "1m23s".
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func resultDuration(r Result) time.Duration { return time.Duration(r.DurationMS) * time.Millisecond }

// cancelState is how far a shutdown requested by the user has progressed.
type cancelState int

const (
	running_ cancelState = iota
	canceling
	killing
)

// progress shows what is happening while commands run. On a terminal it keeps
// one updating status line; otherwise it prints one line per command as it
// finishes. Completion order is nondeterministic: results.json is the ordered
// record. All methods are safe on a nil *progress.
type progress struct {
	mu      sync.Mutex
	w       io.Writer
	opts    outputOptions
	dir     string
	total   int
	done    int
	failed  int
	running map[string]time.Time
	state   cancelState
	width   func() int // current terminal width; re-read on every draw
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

func newProgress(w io.Writer, opts outputOptions, dir string, total int) *progress {
	p := &progress{w: w, opts: opts, dir: dir, total: total, running: map[string]time.Time{}, width: func() int { return termWidth(w) }, stopCh: make(chan struct{})}
	if opts.tty {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					p.mu.Lock()
					p.draw()
					p.mu.Unlock()
				case <-p.stopCh:
					return
				}
			}
		}()
	}
	return p
}

func (p *progress) started(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running[name] = time.Now()
	p.draw()
}

func (p *progress) finished(r Result) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, r.Name)
	p.done++
	if r.Status != StatusOK && r.Status != StatusCanceled {
		p.failed++
	}
	if p.opts.tty {
		if p.state != running_ {
			// Say how each command died while the shutdown is in progress.
			fmt.Fprintf(p.w, "\r\x1b[2K%s: %s %s\n", r.Name, p.opts.paintStatus(r, describe(r)), formatDuration(resultDuration(r)))
		}
		p.draw()
		return
	}
	fmt.Fprintf(p.w, "%s: %s %s %s\n", r.Name, describe(r), formatDuration(resultDuration(r)), p.opts.path(p.dir, r.Log))
}

// cancel tells the user that shutdown is underway (or being forced).
func (p *progress) cancel(s cancelState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s <= p.state {
		return
	}
	p.state = s
	if !p.opts.tty {
		fmt.Fprintln(p.w, "runbox: "+cancelMessage(s))
		return
	}
	p.draw()
}

func cancelMessage(s cancelState) string {
	if s == killing {
		return "killing commands…"
	}
	return "canceling… (Ctrl-C again to force)"
}

// draw redraws the status line. Callers hold p.mu.
func (p *progress) draw() {
	if !p.opts.tty {
		return
	}
	head := fmt.Sprintf("runbox: %d/%d done", p.done, p.total)
	if p.failed > 0 {
		head += fmt.Sprintf(", %d failed", p.failed)
	}
	names := make([]string, 0, len(p.running))
	for n := range p.running {
		names = append(names, n)
	}
	// Longest-running first: it is the one the user is most likely waiting
	// on, and the last to be collapsed into "+N more".
	sort.Slice(names, func(i, j int) bool {
		if a, b := p.running[names[i]], p.running[names[j]]; !a.Equal(b) {
			return a.Before(b)
		}
		return names[i] < names[j]
	})
	for i, n := range names {
		names[i] = fmt.Sprintf("%s(%v)", n, time.Since(p.running[n]).Round(time.Second))
	}
	fmt.Fprint(p.w, "\r\x1b[2K"+p.statusLine(head, names))
}

// statusLine fits the status into one terminal row (a wrapped line couldn't
// be erased by a carriage return), dropping running commands from the end of
// the list in favor of a "+N more" count instead of chopping names.
func (p *progress) statusLine(head string, running []string) string {
	avail := max(p.width()-1, 10)
	paint := func(h string) string { return h }
	if p.failed > 0 {
		failed := fmt.Sprintf("%d failed", p.failed)
		paint = func(h string) string { return strings.Replace(h, failed, p.opts.paint("31", failed), 1) }
	}
	if p.state != running_ {
		msg := cancelMessage(p.state)
		head = "runbox: " + msg
		paint = func(string) string { return "runbox: " + p.opts.paint("33", msg) }
	}
	n := len(running)
	for k := n; k >= 1; k-- {
		tail := " — running: " + strings.Join(running[:k], ", ")
		if k < n {
			tail += fmt.Sprintf(", +%d more", n-k)
		}
		if runeLen(head+tail) <= avail {
			return paint(head) + tail
		}
	}
	if n > 0 {
		if tail := fmt.Sprintf(" — %d running", n); runeLen(head+tail) <= avail {
			return paint(head) + tail
		}
	}
	if runeLen(head) <= avail {
		return paint(head)
	}
	return truncate(head, avail)
}

func runeLen(s string) int { return len([]rune(s)) }

func truncate(s string, w int) string {
	if r := []rune(s); len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s
}

// stop ends the live display and erases the status line.
func (p *progress) stop() {
	if p == nil {
		return
	}
	close(p.stopCh)
	p.wg.Wait()
	if p.opts.tty {
		fmt.Fprint(p.w, "\r\x1b[2K")
	}
}

// printReport writes the final report: on a terminal an aligned table with one
// row per command; then the tail of every unsuccessful command's log, so no
// follow-up read is needed; then a summary line. It reports whether any
// command was unsuccessful. wall is the elapsed time of the whole run.
func printReport(w io.Writer, dir string, results []Result, tail int, wall time.Duration, opts outputOptions) (failed bool) {
	var ok, canceled int
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusCanceled:
			canceled++
		}
	}
	if opts.quiet && ok == len(results) {
		return false
	}
	if opts.tty && !opts.quiet {
		printTable(w, dir, results, opts)
	}
	for _, r := range results {
		if r.Status == StatusOK || tail == 0 {
			continue
		}
		text, clipped, size, err := readTail(filepath.Join(dir, r.Log), tail, tailByteLimit(tail))
		// Only claim "last N lines" when the log was actually cut down to them.
		what := opts.link(filepath.Join(dir, r.Log), r.Log)
		if strings.Count(text, "\n")+1 >= tail && text != "" {
			what = fmt.Sprintf("last %d lines of %s", tail, what)
		}
		fmt.Fprintf(w, "\n--- %s: %s (%s) ---\n", r.Name, describe(r), what)
		switch {
		case err != nil:
			fmt.Fprintf(w, "(cannot read log: %v)\n", err)
		case text == "":
			fmt.Fprintln(w, "(log is empty)")
		default:
			if clipped {
				fmt.Fprintf(w, "(log is %s; only its last %s were read, so the output may start mid-line)\n", humanBytes(size), humanBytes(tailByteLimit(tail)))
			}
			fmt.Fprintln(w, text)
		}
	}
	summary := fmt.Sprintf("%d/%d ok, %d failed", ok, len(results), len(results)-ok-canceled)
	if canceled > 0 {
		summary += fmt.Sprintf(", %d canceled", canceled)
	}
	fmt.Fprintf(w, "\nsummary: %s in %s; results: %s\n", summary, formatDuration(wall), opts.pathLink(dir, "results.json"))
	return ok != len(results)
}

func printTable(w io.Writer, dir string, results []Result, opts outputOptions) {
	var nameW, statusW, durW int
	durs := make([]string, len(results))
	for i, r := range results {
		durs[i] = formatDuration(resultDuration(r))
		nameW = max(nameW, len(r.Name)+1)
		statusW = max(statusW, len(describe(r)))
		durW = max(durW, len(durs[i]))
	}
	for i, r := range results {
		status := describe(r)
		pad := strings.Repeat(" ", statusW-len(status))
		fmt.Fprintf(w, "%-*s %s%s  %*s  %s\n", nameW, r.Name+":", opts.paintStatus(r, status), pad, durW, durs[i], opts.pathLink(dir, r.Log))
	}
}

// readTail returns the last n lines of the file, looking at no more than the
// last maxBytes bytes. clipped reports that the file was larger than that
// window and the result may therefore be incomplete or start mid-line.
func readTail(path string, n int, maxBytes int64) (text string, clipped bool, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false, 0, err
	}
	size = st.Size()
	off := max(size-maxBytes, 0)
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return "", false, size, err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 {
		if len(lines) > 1 {
			lines = lines[1:] // first line is probably cut off
		}
		clipped = len(lines) < n || len(lines) == 1
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), clipped, size, nil
}

// tailByteLimit is the most bytes read from a log to show n lines.
func tailByteLimit(n int) int64 {
	if n > maxTailBytes/tailBytesPerLine {
		return maxTailBytes
	}
	return int64(n) * tailBytesPerLine
}
