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
rest is passed to `sh -c`. Names may contain letters, digits, `.`, `_` and `-`
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
* `--tail N`: show the last N lines of the log of each failed command (default 15, `0` disables).

## Output

When stdout is piped (e.g. read by an agent) runbox prints one line per command
as it finishes (completion order, not argument order), then the last lines of
every unsuccessful command's log, then a summary with the total wall-clock time.
On a terminal it instead shows a live `1/4 done, 1 failed — running: build(12s)`
line and finishes with an aligned, colored table. `results.json` is the stable,
ordered record in both cases.

Piped (e.g. `runbox ... | cat`), for `--run 'build=...' --run 'lint=...' --run 'test=...'`:

```text
runbox: running 3 commands, logs in /work/.runbox/run-1
lint: FAILED exit=1 1ms /work/.runbox/run-1/lint.log
build: OK 1.0s /work/.runbox/run-1/build.log
test: OK 2.0s /work/.runbox/run-1/test.log

--- lint: FAILED exit=1 (lint.log) ---
lint: E501 line too long

summary: 2/3 ok, 1 failed in 2.0s; results: /work/.runbox/run-1/results.json
```

On a terminal, after the live line has been erased:

```text
runbox: running 3 commands, logs in .runbox/run-2 (.runbox/latest)
build: OK             1.0s  build.log
lint:  FAILED exit=1   1ms  lint.log
test:  OK             2.0s  test.log

--- lint: FAILED exit=1 (lint.log) ---
lint: E501 line too long

summary: 2/3 ok, 1 failed in 2.0s; results: results.json
```

Color is used only on a terminal. Set [`NO_COLOR`](https://no-color.org) (to any non-empty
value) to disable it, or `TERM=dumb` to get the plain piped-style output on a terminal. Piped output
never contains escape codes.

Statuses: `OK`, `FAILED exit=N`, `FAILED (could not start)`, `SIGNALED (TERM)`
(killed by a signal) and `CANCELED` (stopped by runbox after Ctrl-C/SIGTERM).
When a log is too large to read in full, the report says so.

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

### results.json

The machine-readable record of a run, for scripts and other tools. It is at
`.runbox/latest/results.json` (or `.runbox/run-N/results.json`) and is written once all
commands have finished, including after Ctrl-C. It is a JSON array with one object per
command, in the order the `--run` options were given:

```json
[
  {"name": "build", "command": "go build ./...", "status": "ok", "exit_code": 0, "duration_ms": 2502, "log": "build.log"},
  {"name": "lint", "command": "ruff check .", "status": "failed", "exit_code": 1, "duration_ms": 830, "log": "lint.log"}
]
```

* `status` is `ok`, `failed`, `signaled` (killed by a signal not sent by runbox) or
  `canceled` (stopped by runbox after Ctrl-C/SIGTERM).
* `exit_code` is `-1` if the command did not exit normally: it could not be started, or it was
  killed by a signal (including by runbox when canceled). Check `status` first.
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
