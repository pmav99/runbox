// Command runbox runs several shell commands concurrently and saves each
// command's output to a log file inside a numbered run directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultShell = "sh"
	baseDir      = ".runbox"
	keepRuns     = 10
)

// version is set at build time with -ldflags "-X main.version=..."; when empty
// it falls back to the module version embedded by `go install`.
var version string

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Command is a named shell command to run.
type Command struct {
	Name  string
	Shell string
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// parseCommand parses a "name=shell command" argument.
func parseCommand(arg string) (Command, error) {
	name, shell, ok := strings.Cut(arg, "=")
	if !ok {
		return Command{}, fmt.Errorf("invalid --run %q: expected NAME=COMMAND", arg)
	}
	if !validName.MatchString(name) {
		return Command{}, fmt.Errorf("invalid command name %q: use letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	if strings.TrimSpace(shell) == "" {
		return Command{}, fmt.Errorf("command %q is empty", name)
	}
	return Command{Name: name, Shell: shell}, nil
}

// parseCommands parses all --run arguments and rejects duplicate names.
func parseCommands(args []string) ([]Command, error) {
	seen := map[string]bool{}
	var cmds []Command
	for _, a := range args {
		c, err := parseCommand(a)
		if err != nil {
			return nil, err
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("duplicate command name %q", c.Name)
		}
		seen[c.Name] = true
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil, errors.New("at least one --run NAME=COMMAND is required")
	}
	return cmds, nil
}

const usage = `Usage: runbox --run NAME=COMMAND [--run NAME=COMMAND ...]

Runs all commands concurrently through SHELL -c (default sh) and saves each one's output to
.runbox/run-N/NAME.log (.runbox/latest points at the newest run).

Options:
  --run NAME=COMMAND  command to run; may be repeated
  --shell PATH        shell used to run the commands; must accept -c COMMAND
                      (default sh, looked up in PATH)
  --timeout SECONDS   stop any command still running after this many seconds
                      (SIGTERM, then SIGKILL after 5s); 0 means no limit (default)
  --tail N            lines of log to show for each failed command (default 15, 0 disables)
  --paths abs|base    print log paths absolute or relative to the run directory
                      (default: absolute when piped, relative on a terminal)
  --quiet, -q         print nothing on success; on failure print only the failed
                      commands' log tails and the summary
  --version           print version and exit
  --help              show this help

Example:
  runbox --run 'build=go build ./...' --run 'lint=ruff check .'
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	var runArgs []string
	fs := flag.NewFlagSet("runbox", flag.ContinueOnError)
	tail := fs.Int("tail", defaultTail, "number of log lines to show for each failed command (0 to disable)")
	paths := fs.String("paths", "", "how log paths are printed: abs or base (default: abs when piped, base on a terminal)")
	shell := fs.String("shell", defaultShell, "path of the shell used to run the commands; must accept -c")
	timeout := fs.Int("timeout", 0, "seconds after which a running command is stopped (0 for no limit)")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	fs.BoolVar(quiet, "q", false, "shorthand for --quiet")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Func("run", "`NAME=COMMAND` to run; may be repeated", func(s string) error {
		runArgs = append(runArgs, s)
		return nil
	})
	fs.SetOutput(io.Discard) // we print our own usage and errors
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usage)
			return 0
		}
		msg := strings.Replace(err.Error(), "flag provided but not defined: -", "unknown option --", 1)
		fmt.Fprintf(os.Stderr, "runbox: %s\n\n%s", msg, usage)
		return 2
	}
	if *showVersion {
		fmt.Println("runbox", versionString())
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "runbox: unexpected argument %q (commands must be passed as --run NAME=COMMAND)\n", fs.Arg(0))
		return 2
	}
	if *tail < 0 {
		fmt.Fprintln(os.Stderr, "runbox: --tail must not be negative")
		return 2
	}
	if *timeout < 0 {
		fmt.Fprintln(os.Stderr, "runbox: --timeout must not be negative")
		return 2
	}
	if *paths != "" && *paths != pathsAbs && *paths != pathsBase {
		fmt.Fprintln(os.Stderr, "runbox: --paths must be abs or base")
		return 2
	}
	if err := checkShell(*shell); err != nil {
		fmt.Fprintln(os.Stderr, "runbox:", err)
		return 2
	}
	cmds, err := parseCommands(runArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runbox:", err)
		return 2
	}

	dir, n, err := createRunDir(baseDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runbox:", err)
		return 1
	}
	// Failing to update the symlink or prune old runs must not fail the run.
	if err := updateLatest(baseDir, n); err != nil {
		fmt.Fprintln(os.Stderr, "runbox: warning:", err)
	}
	if err := prune(baseDir, keepRuns, n); err != nil {
		fmt.Fprintln(os.Stderr, "runbox: warning:", err)
	}

	// The first signal cancels the commands (SIGTERM); a second one makes
	// runbox SIGKILL them immediately instead of waiting out the grace period.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	force := make(chan struct{})
	var received atomic.Int32
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var prog *progress // set below, before any signal handling matters
	var progMu sync.Mutex
	getProg := func() *progress { progMu.Lock(); defer progMu.Unlock(); return prog }
	go func() {
		sig := <-sigs
		received.Store(int32(sig.(syscall.Signal)))
		cancel()
		getProg().cancel(canceling)
		<-sigs
		close(force)
		getProg().cancel(killing)
	}()

	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	opts := newOutputOptions(os.Stdout, *paths)
	opts.quiet = *quiet
	shown := dir
	if opts.paths == pathsBase {
		shown = fmt.Sprintf("%s/%s (%s/latest)", baseDir, filepath.Base(dir), baseDir)
	}
	shown = opts.link(dir, shown)
	if !opts.quiet {
		noun := "commands"
		if len(cmds) == 1 {
			noun = "command"
		}
		fmt.Printf("runbox: running %d %s, logs in %s\n", len(cmds), noun, shown)
		progMu.Lock()
		prog = newProgress(os.Stdout, opts, dir, len(cmds))
		progMu.Unlock()
	}
	start := time.Now()
	results := runAll(ctx, force, dir, *shell, time.Duration(*timeout)*time.Second, cmds, getProg())
	getProg().stop()
	wall := time.Since(start)

	if err := writeResults(dir, results); err != nil {
		fmt.Fprintln(os.Stderr, "runbox:", err)
		return 1
	}
	failed := printReport(os.Stdout, dir, results, *tail, wall, opts)
	if sig := received.Load(); sig != 0 {
		return 128 + int(sig) // conventional "killed by signal" exit status
	}
	if failed {
		return 1
	}
	return 0
}

// checkShell verifies that shell is an executable regular file, so a typo is
// reported once instead of as a start failure of every command. The default
// "sh" is the one name that is looked up in PATH; any other value is a path.
func checkShell(shell string) error {
	if shell == defaultShell {
		return nil
	}
	fi, err := os.Stat(shell)
	if err != nil {
		return fmt.Errorf("invalid --shell: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("invalid --shell %q: not an executable file", shell)
	}
	return nil
}

func writeResults(dir string, results []Result) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "results.json"), append(data, '\n'), 0o644)
}
