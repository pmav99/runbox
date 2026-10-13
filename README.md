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
* `results.json` has one entry per command: `name`, `command`, `status`
  (`ok`, `failed`, `signaled` or `canceled`), `exit_code` (`-1` if the command could not be
  started or was killed by a signal), optional `signal` and `error`, `duration_ms` and `log`.

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
