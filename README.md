# diskmap

Fast disk usage scanner for macOS. Scans a directory in parallel and either serves a
WinDirStat-style treemap report in the browser or prints a structured JSON analysis.

- **Fast**: parallel goroutines + `getattrlistbulk(2)`, one syscall per batch of directory
  entries instead of one `lstat` per file. ~2 s for 500k files (warm cache).
- **Accurate**: sizes are allocated bytes on disk (like `du`), hard links are counted once,
  and the scan stays on the filesystem of the given path.
- **Single binary**: the HTML report is embedded, no runtime dependencies.

## Install

Requires Go 1.25+ and Xcode command line tools (cgo).

```bash
make install            # builds and copies to ~/.local/bin (override with PREFIX=...)
```

## Usage

```bash
diskmap ~/Library                       # scan, write report, serve on :8765, open the browser
diskmap ~ -out ~/diskreport -no-serve   # just write index.html
diskmap ~ -json -top 50 -depth 3 > report.json
```

| Flag | Default | Description |
|---|---|---|
| `-threads` | 16 | parallel directory scanners |
| `-port` | 8765 | port for the report server (falls back to a free one) |
| `-out` | temp dir | directory to write `index.html` into |
| `-no-serve` | false | only write the report |
| `-detail` | 200000 | approx. max treemap tiles; smaller items are lumped |
| `-json` | false | print a JSON analysis to stdout instead of the HTML report |
| `-top` | 50 | with `-json`: number of largest files/directories |
| `-depth` | 3 | with `-json`: max depth for the largest-directories list |

Flags may come before or after the path. Progress is written to stderr.

### HTML report

- Folder tree with size bars, percentages and file counts
- Extension table; click an extension to highlight its files in the treemap
- Treemap coloured by extension; hover for details, click to select,
  double-click to zoom into a folder, Esc or the breadcrumb to zoom out

### JSON analysis

| Field | Contents |
|---|---|
| `volume` | total/free bytes and % used of the volume holding the path |
| `total_bytes`, `files`, `dirs`, `unreadable_dirs` | scan totals |
| `top_files[]` | largest files: path, size, extension, modified date, age in days |
| `top_dirs[]` | largest directories up to `-depth`, with newest-file date and age |
| `regenerable_dirs[]` | largest `node_modules`, `.venv`, `__pycache__`, `.next`, `DerivedData`, `.gradle`, `Pods`, … (outermost match only) |
| `regenerable_totals[]` | the same, aggregated per kind |
| `extensions[]` | top 30 extensions by size |

## Development

```bash
make            # vet + tests (with -race) + JS syntax check + build
make test
make run TARGET=~/Downloads
make clean
```

Layout:

- `main.go`: CLI, parallel scanner, tree rollup/pruning, HTML report and server
- `listdir_darwin.go`: `getattrlistbulk` directory reader (falls back to readdir + lstat)
- `analyze.go`: top-k files and the JSON analysis
- `template.html`: the report page (embedded at build time)

macOS only.
