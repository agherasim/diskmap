package main

import (
	"container/heap"
	"fmt"
	"sort"
	"syscall"
	"time"
)

// fileRec is one file remembered for the largest-files list.
type fileRec struct {
	path  string
	size  int64
	ext   string
	mtime int64
}

// topFiles keeps the k largest files seen by one worker (a min-heap by size).
type topFiles struct {
	k    int
	recs []fileRec
}

func (t *topFiles) Len() int           { return len(t.recs) }
func (t *topFiles) Less(i, j int) bool { return t.recs[i].size < t.recs[j].size }
func (t *topFiles) Swap(i, j int)      { t.recs[i], t.recs[j] = t.recs[j], t.recs[i] }
func (t *topFiles) Push(x any)         { t.recs = append(t.recs, x.(fileRec)) }
func (t *topFiles) Pop() any {
	r := t.recs[len(t.recs)-1]
	t.recs = t.recs[:len(t.recs)-1]
	return r
}

func (t *topFiles) add(r fileRec) {
	switch {
	case t.k <= 0 || r.size <= 0:
	case len(t.recs) < t.k:
		heap.Push(t, r)
	case r.size > t.recs[0].size:
		t.recs[0] = r
		heap.Fix(t, 0)
	}
}

// mergeTop combines the per-worker lists into the k largest, biggest first.
func mergeTop(recs []fileRec, k int) []fileRec {
	sort.Slice(recs, func(i, j int) bool { return recs[i].size > recs[j].size })
	if len(recs) > k {
		recs = recs[:k]
	}
	return recs
}

// knownDirs are directory names that hold regenerable data (dependencies,
// build output, tool caches). Matched by exact name; nested matches inside an
// already-matched directory are not reported again.
var knownDirs = map[string]string{
	"node_modules":  "npm/pnpm/yarn dependencies",
	".venv":         "Python virtualenv",
	"venv":          "Python virtualenv",
	"__pycache__":   "Python bytecode cache",
	".pytest_cache": "pytest cache",
	".mypy_cache":   "mypy cache",
	".ruff_cache":   "ruff cache",
	".tox":          "tox environments",
	".next":         "Next.js build output",
	".nuxt":         "Nuxt build output",
	".turbo":        "Turborepo cache",
	".parcel-cache": "Parcel cache",
	".svelte-kit":   "SvelteKit build output",
	".astro":        "Astro build cache",
	"DerivedData":   "Xcode build output",
	".gradle":       "Gradle cache",
	".terraform":    "Terraform providers/modules",
	"Pods":          "CocoaPods dependencies",
	".dart_tool":    "Dart/Flutter tool cache",
}

type Report struct {
	Root        string      `json:"root"`
	ScannedAt   string      `json:"scanned_at"`
	Seconds     float64     `json:"scan_seconds"`
	Volume      *VolumeInfo `json:"volume,omitempty"`
	TotalBytes  int64       `json:"total_bytes"`
	Total       string      `json:"total"`
	Files       int64       `json:"files"`
	Dirs        int64       `json:"dirs"`
	Unreadable  int64       `json:"unreadable_dirs"`
	TopFiles    []FileOut   `json:"top_files"`
	TopDirs     []DirOut    `json:"top_dirs"`
	KnownDirs   []KnownOut  `json:"regenerable_dirs"`
	KnownTotals []KindTotal `json:"regenerable_totals"`
	Extensions  []ExtOut    `json:"extensions"`
}

type VolumeInfo struct {
	TotalBytes int64   `json:"total_bytes"`
	FreeBytes  int64   `json:"free_bytes"`
	Free       string  `json:"free"`
	UsedPct    float64 `json:"used_pct"`
}

type FileOut struct {
	Path     string  `json:"path"`
	Bytes    int64   `json:"bytes"`
	Size     string  `json:"size"`
	Pct      float64 `json:"pct_of_scan"`
	Ext      string  `json:"ext"`
	Modified string  `json:"modified"`
	AgeDays  int     `json:"age_days"`
}

type DirOut struct {
	Path      string  `json:"path"`
	Depth     int     `json:"depth"`
	Bytes     int64   `json:"bytes"`
	Size      string  `json:"size"`
	Pct       float64 `json:"pct_of_scan"`
	Files     int64   `json:"files"`
	NewestMod string  `json:"newest_file_modified"`
	AgeDays   int     `json:"newest_file_age_days"`
}

type KnownOut struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Size      string `json:"size"`
	NewestMod string `json:"newest_file_modified"`
	AgeDays   int    `json:"newest_file_age_days"`
}

type KindTotal struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
	Size  string `json:"size"`
}

type ExtOut struct {
	Ext   string  `json:"ext"`
	Bytes int64   `json:"bytes"`
	Size  string  `json:"size"`
	Pct   float64 `json:"pct_of_scan"`
	Files int64   `json:"files"`
}

func human(b int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f, i := float64(b), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func pctOf(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(int(float64(a)/float64(b)*10000)) / 100
}

func dateAge(mtime int64, now time.Time) (string, int) {
	if mtime <= 0 {
		return "", -1
	}
	t := time.Unix(mtime, 0)
	return t.Format("2006-01-02"), int(now.Sub(t).Hours() / 24)
}

func volumeInfo(path string) *VolumeInfo {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return nil
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	return &VolumeInfo{TotalBytes: total, FreeBytes: free, Free: human(free), UsedPct: 100 - pctOf(free, total)}
}

// analyze turns a scanned (un-pruned) tree into the JSON report.
func analyze(root string, tree *Node, s *scanner, topK, maxDepth int, elapsed time.Duration) Report {
	now := time.Now()
	r := Report{
		Root: root, ScannedAt: now.Format(time.RFC3339), Seconds: float64(elapsed.Milliseconds()/100) / 10,
		Volume: volumeInfo(root), TotalBytes: tree.Size, Total: human(tree.Size),
		Files: s.files.Load(), Dirs: s.dirs.Load(), Unreadable: s.errs.Load(),
		TopFiles: []FileOut{}, TopDirs: []DirOut{}, KnownDirs: []KnownOut{}, KnownTotals: []KindTotal{},
	}

	for _, f := range s.top {
		d, age := dateAge(f.mtime, now)
		r.TopFiles = append(r.TopFiles, FileOut{f.path, f.size, human(f.size), pctOf(f.size, tree.Size), f.ext, d, age})
	}

	totals := map[string]*KindTotal{}
	var walk func(n *Node, depth int, inKnown bool)
	walk = func(n *Node, depth int, inKnown bool) {
		for _, c := range n.Children {
			if c.path == "" { // files have no path
				continue
			}
			d, age := dateAge(c.mtime, now)
			if depth+1 <= maxDepth {
				r.TopDirs = append(r.TopDirs, DirOut{c.path, depth + 1, c.Size, human(c.Size), pctOf(c.Size, tree.Size), c.Files, d, age})
			}
			kind, known := knownDirs[c.Name]
			if known && !inKnown && c.Size > 0 {
				r.KnownDirs = append(r.KnownDirs, KnownOut{kind, c.path, c.Size, human(c.Size), d, age})
				t := totals[kind]
				if t == nil {
					t = &KindTotal{Kind: kind}
					totals[kind] = t
				}
				t.Count++
				t.Bytes += c.Size
			}
			walk(c, depth+1, inKnown || known)
		}
	}
	walk(tree, 0, false)

	sort.Slice(r.TopDirs, func(i, j int) bool { return r.TopDirs[i].Bytes > r.TopDirs[j].Bytes })
	if len(r.TopDirs) > topK {
		r.TopDirs = r.TopDirs[:topK]
	}
	sort.Slice(r.KnownDirs, func(i, j int) bool { return r.KnownDirs[i].Bytes > r.KnownDirs[j].Bytes })
	if len(r.KnownDirs) > topK {
		r.KnownDirs = r.KnownDirs[:topK]
	}
	for _, t := range totals {
		t.Size = human(t.Bytes)
		r.KnownTotals = append(r.KnownTotals, *t)
	}
	sort.Slice(r.KnownTotals, func(i, j int) bool { return r.KnownTotals[i].Bytes > r.KnownTotals[j].Bytes })

	for k, v := range s.exts {
		r.Extensions = append(r.Extensions, ExtOut{k, v.bytes, human(v.bytes), pctOf(v.bytes, tree.Size), v.count})
	}
	sort.Slice(r.Extensions, func(i, j int) bool { return r.Extensions[i].Bytes > r.Extensions[j].Bytes })
	if len(r.Extensions) > 30 {
		r.Extensions = r.Extensions[:30]
	}
	return r
}
