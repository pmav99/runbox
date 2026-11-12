# runbox

Run several shell commands concurrently and keep each one's output in a log file.

## Install

Requires Go 1.24+ and a Unix-like OS (Linux, macOS) with `sh`.

```bash
go install github.com/pmav99/runbox@latest
# or, from a checkout:
go build -o runbox .   # or: make build
```

## Usage

```bash
runbox \
  --run 'build=go build ./...' \
  --run 'lint=ruff check .' \
  --run 'format=ruff format --check .'
```

Each `--run` is `NAME=COMMAND`. The text before the first `=` is the name; the
rest is passed to `sh -c` (see `--shell`). Names may contain letters, digits, `.`, `_` and `-`
(and must start with a letter or digit), and must be unique.

All commands start at once. stdout and stderr of each command go to one log file.
`runbox` exits with `0` if every command succeeded, `1` if any failed, `2` on usage
errors, and `128+N` (130 for Ctrl-C, 143 for SIGTERM) if it was interrupted.

Options:

* `--help`: show usage.
* `--version`: print the version and exit.
* `--paths abs|base`: print log paths absolute or relative to the run directory
  (default: absolute when stdout is piped, relative on a terminal).
* `--quiet`, `-q`: print nothing when everything succeeds. On failure, print only the
  failed commands' log tails and the summary. `results.json` and exit codes are unchanged.
* `--shell PATH`: run the commands with this shell instead of `sh`. It is used as
  `PATH -c COMMAND`, so it must accept `-c` (bash, zsh, dash, ...). The value is a path to an
  executable, e.g. `--shell /bin/bash`; it is not looked up in `PATH`. The default `sh` is.
  Extra shell arguments are not supported. Linux is the primary target, macOS best effort.
* `--timeout SECONDS`: stop any command that is still running after this many seconds
  (a whole number; default `0`, no limit). The command's process group gets `SIGTERM`, and
  `SIGKILL` after 5 more seconds, as for [cancellation](#cancellation). It is recorded as `timed_out`
  and counts as failed.
* `--tail N`: show the last N lines of the log of each failed command (default 15, `0` disables).
  Each failure header shows the log's total line count and how many lines are displayed.

## Output

When stdout is piped, runbox prints a status line for each command as it finishes,
followed immediately by its log tail if it was unsuccessful. On a terminal it
shows a live `1/4 done, 1 failed — running: build(12s)` line, prints unsuccessful
log tails as soon as those commands finish, and ends with an aligned, colored
table of all command statuses. This lets you inspect a fast failure while other
commands are still running without repeating status lines in the terminal.
Both styles finish with a summary and total wall-clock time. Completion order
may vary; `results.json` is the stable, ordered record.

Piped (e.g. `runbox ... | cat`), for `--run 'build=...' --run 'lint=...' --run 'test=...'`:

```text
runbox: running 3 commands, logs in /work/.runbox/run-1
lint: FAILED exit=1 1ms /work/.runbox/run-1/lint.log

--- lint: FAILED exit=1 (1 line in lint.log) ---
lint: E501 line too long

build: OK 1.0s /work/.runbox/run-1/build.log
test: OK 2.0s /work/.runbox/run-1/test.log

summary: 2/3 ok, 1 failed in 2.0s; results: /work/.runbox/run-1/results.json
```

On a terminal, after the live line has been erased:

```text
runbox: running 3 commands, logs in .runbox/run-2 (.runbox/latest)

--- lint: FAILED exit=1 (1 line in lint.log) ---
lint: E501 line too long
```

The final recap on a terminal is:

```text
build: OK             1.0s  build.log
lint:  FAILED exit=1   1ms  lint.log
test:  OK             2.0s  test.log

summary: 2/3 ok, 1 failed in 2.0s; results: results.json
```

Color is used only on a terminal. Set [`NO_COLOR`](https://no-color.org) (to any non-empty
value) to disable it, or `TERM=dumb` to get the plain piped-style output on a terminal. Piped output
never contains escape codes.

Statuses: `OK`, `FAILED exit=N`, `FAILED (could not start)`, `SIGNALED (TERM)`
(killed by a signal) and `CANCELED` (stopped by runbox after Ctrl-C/SIGTERM).
When a log is too large to read in full, the report says so.
Log line counts include blank lines and are not counts of tool errors.

### Run directory

Each invocation creates a new directory in `./.runbox`:

```text
.runbox/
├── latest -> run-12
├── run-12/
│   ├── build.log
│   ├── lint.log
│   ├── format.log
│   └── results.json
├── run-11/
└── run-10/
```

* Run numbers increase by one per invocation. Directories are created atomically,
  so simultaneous invocations get different numbers.
* `latest` is a symlink to the newest run.
* Only the 10 newest runs are kept; older ones are deleted automatically.
* `results.json` is described [below](#resultsjson).

### Command environment

Each command inherits runbox's environment, plus:

* `RUNBOX_RUN_DIR`: the absolute path of this invocation's run directory, e.g. `/work/.runbox/run-12`.
* `RUNBOX_NAME`: the command's name, the `NAME` in `--run NAME=COMMAND`.

Use them to keep a command's other output files next to its log:

```bash
runbox --run 'test=pytest --junitxml="$RUNBOX_RUN_DIR/$RUNBOX_NAME-junit.xml"'
```

Put the `--run` value in single quotes so that the command's shell expands the variables,
not yours. Use `RUNBOX_RUN_DIR` rather than `.runbox/latest`: `latest` moves as soon as
another invocation starts, and `.runbox` is relative to the directory runbox was started in.

* Files written there are deleted along with the run when it is pruned.
* Don't create `.log` files or `results.json` there; runbox uses those names.
* Values inherited from an outer runbox are replaced, so nested invocations get their own.
  runbox itself does not read these variables.

### results.json

The machine-readable record of a run, for scripts and other tools. It is at
`.runbox/latest/results.json` (or `.runbox/run-N/results.json`) and is written once all
commands have finished, including after Ctrl-C. It is a JSON array with one object per
command, in the order the `--run` options were given:

```json
[
  {"name": "build", "command": "go build ./...", "shell": "sh", "status": "ok", "exit_code": 0, "duration_ms": 2502, "log": "build.log"},
  {"name": "lint", "command": "ruff check .", "shell": "sh", "status": "failed", "exit_code": 1, "duration_ms": 830, "log": "lint.log"}
]
```

* `status` is `ok`, `failed`, `timed_out` (stopped by runbox after `--timeout`), `signaled` (killed by a signal not sent by runbox) or
  `canceled` (stopped by runbox after Ctrl-C/SIGTERM).
* `exit_code` is `-1` if the command did not exit normally: it could not be started, or it was
  killed by a signal (including by runbox when canceled). Check `status` first.
* `shell` is the `--shell` value the command was run with (`sh` by default).
* `log` is relative to the run directory.
* `signal` (e.g. `"TERM"`, only when `signaled`) and `error` (only when the command
  could not be started) are omitted otherwise.

For a script, runbox's own exit status says whether everything succeeded; read
`results.json` to find out which commands did not.

## Cancellation

On `SIGINT` (Ctrl-C) or `SIGTERM`, `runbox` sends `SIGTERM` to every running
command's process group (so children of the shell are included), and `SIGKILL`
if it hasn't exited after 5 seconds. A second signal skips the wait and sends
`SIGKILL` immediately. Canceled commands are recorded as
`canceled`, and `results.json` is still written.

## Development

```bash
make test    # go test -race ./...
make vet
make build
make install # go install
make clean
```
