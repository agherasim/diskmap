// diskmap scans a directory and serves a WinDirStat-style treemap report.
//
// Usage:
//
//	diskmap PATH [-port 8765] [-out DIR] [-detail 200000] [-threads 16] [-no-serve]
//	diskmap PATH -json [-top 50] [-depth 3]   # structured analysis on stdout
//
// Sizes are allocated bytes on disk (like du), hard links are counted once,
// and the scan stays on the filesystem of PATH.
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed template.html
var template string

const keepFilesPerDir = 200 // keeps memory bounded on huge dirs; the rest is lumped

type entry struct {
	name  string
	dev   int64
	dir   bool
	ino   uint64
	nlink uint32
	size  int64
	mtime int64 // unix seconds
}

type Node struct {
	Name     string  `json:"n"`
	Size     int64   `json:"s"`
	Files    int64   `json:"f,omitempty"`
	Ext      string  `json:"e,omitempty"`
	Children []*Node `json:"c,omitempty"`

	path  string
	dirs  []*Node
	mtime int64 // files: own; directories: newest file in the subtree
}

type extStat struct{ bytes, count int64 }

type scanner struct {
	rootDev int64

	mu      sync.Mutex
	cond    *sync.Cond
	stack   []*Node // LIFO keeps the backlog small (depth-first-ish)
	pending int     // queued + in-progress directories

	seenMu sync.Mutex
	seen   map[[2]uint64]bool

	extMu sync.Mutex
	exts  map[string]*extStat

	topK  int // how many of the largest files to remember (0 = none)
	topMu sync.Mutex
	top   []fileRec

	files, dirs, errs atomic.Int64
}

// listDirStat is the portable fallback: readdir + lstat per entry.
func listDirStat(path string) ([]entry, error) {
	des, err := os.ReadDir(path)
	out := make([]entry, 0, len(des))
	for _, d := range des {
		var st syscall.Stat_t
		if syscall.Lstat(filepath.Join(path, d.Name()), &st) != nil {
			continue
		}
		e := entry{name: d.Name(), dev: int64(st.Dev), dir: st.Mode&syscall.S_IFMT == syscall.S_IFDIR, ino: st.Ino, mtime: st.Mtimespec.Sec}
		if !e.dir { // like getattrlistbulk: file attributes only for non-directories
			e.nlink, e.size = uint32(st.Nlink), st.Blocks*512
		}
		out = append(out, e)
	}
	return out, err
}

func (s *scanner) push(nodes []*Node) {
	s.mu.Lock()
	s.stack = append(s.stack, nodes...)
	s.pending += len(nodes)
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *scanner) pop() *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.stack) == 0 && s.pending > 0 {
		s.cond.Wait()
	}
	if len(s.stack) == 0 {
		return nil
	}
	n := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	return n
}

func (s *scanner) finish() {
	s.mu.Lock()
	s.pending--
	done := s.pending == 0
	s.mu.Unlock()
	if done {
		s.cond.Broadcast()
	}
}

func (s *scanner) worker() {
	buf := make([]byte, 256<<10)
	top := &topFiles{k: s.topK}
	for n := s.pop(); n != nil; n = s.pop() {
		s.scanDir(n, buf, top)
		s.push(n.dirs)
		s.finish()
	}
	s.topMu.Lock()
	s.top = append(s.top, top.recs...)
	s.topMu.Unlock()
}

func (s *scanner) scanDir(n *Node, buf []byte, top *topFiles) {
	ents, err := listDir(n.path, buf)
	if err != nil {
		s.errs.Add(1)
	}
	var files []*Node
	exts := map[string]*extStat{}
	for _, e := range ents {
		p := n.path + "/" + e.name
		if strings.HasSuffix(n.path, "/") {
			p = n.path + e.name
		}
		if e.dir {
			if e.dev == s.rootDev {
				n.dirs = append(n.dirs, &Node{Name: e.name, path: p})
			}
			continue
		}
		size := e.size
		if e.nlink > 1 {
			key := [2]uint64{uint64(e.dev), e.ino}
			s.seenMu.Lock()
			if s.seen[key] {
				size = 0
			}
			s.seen[key] = true
			s.seenMu.Unlock()
		}
		ext := extOf(e.name)
		st := exts[ext]
		if st == nil {
			st = &extStat{}
			exts[ext] = st
		}
		st.bytes += size
		st.count++
		files = append(files, &Node{Name: e.name, Size: size, Ext: ext, mtime: e.mtime})
		top.add(fileRec{p, size, ext, e.mtime})
	}

	n.Files = int64(len(files))
	if len(files) > keepFilesPerDir {
		sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })
		lump := &Node{Name: fmt.Sprintf("<%d smaller files>", len(files)-keepFilesPerDir), Ext: "*"}
		for _, f := range files[keepFilesPerDir:] {
			lump.Size += f.Size
			lump.mtime = max(lump.mtime, f.mtime)
		}
		files = append(files[:keepFilesPerDir:keepFilesPerDir], lump)
	}
	n.Children = files // subdirectories are prepended in rollup()

	s.extMu.Lock()
	for k, v := range exts {
		t := s.exts[k]
		if t == nil {
			t = &extStat{}
			s.exts[k] = t
		}
		t.bytes += v.bytes
		t.count += v.count
	}
	s.extMu.Unlock()
	s.files.Add(n.Files)
	s.dirs.Add(int64(len(n.dirs)))
}

// extOf returns the lowercased extension; leading dots belong to the name,
// so ".bashrc" has none. Hash-like suffixes of cache files (".d4835…") are not
// extensions either: they'd flood the extension table and use up colours.
func extOf(name string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimLeft(name, ".")))
	if ext == "" || len(ext) > 16 {
		return "(none)"
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '+') {
			return "(none)"
		}
	}
	return ext
}

// rollup attaches subdirectories and sums sizes, file counts and newest
// modification time bottom-up.
func rollup(n *Node) {
	for _, d := range n.dirs {
		rollup(d)
		n.Files += d.Files
	}
	n.Children = append(n.dirs, n.Children...)
	n.dirs = nil
	for _, c := range n.Children {
		n.Size += c.Size
		n.mtime = max(n.mtime, c.mtime)
	}
}

// prune lumps siblings smaller than threshold into one node and sorts by size.
func prune(n *Node, threshold int64) {
	if n.Children == nil {
		return
	}
	var big []*Node
	lump := &Node{Ext: "*"}
	small := 0
	for _, c := range n.Children {
		switch {
		case c.Size >= threshold:
			big = append(big, c)
		case c.Size > 0:
			lump.Size += c.Size
			small++
		}
	}
	if small > 0 {
		lump.Name = fmt.Sprintf("<%d small items>", small)
		big = append(big, lump)
	}
	sort.Slice(big, func(i, j int) bool { return big[i].Size > big[j].Size })
	n.Children = big
	for _, c := range big {
		prune(c, threshold)
	}
}

func scan(root string, threads, topK int) (*Node, *scanner) {
	var st syscall.Stat_t
	if err := syscall.Lstat(root, &st); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s := &scanner{rootDev: int64(st.Dev), seen: map[[2]uint64]bool{}, exts: map[string]*extStat{}, topK: topK}
	s.cond = sync.NewCond(&s.mu)
	s.dirs.Store(1)
	tree := &Node{Name: root, path: root}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "\r  %d files, %d dirs scanned", s.files.Load(), s.dirs.Load())
			}
		}
	}()

	s.push([]*Node{tree})
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.worker() }()
	}
	wg.Wait()
	close(stop)
	fmt.Fprintf(os.Stderr, "\r  %d files, %d dirs scanned\n", s.files.Load(), s.dirs.Load())
	rollup(tree)
	s.top = mergeTop(s.top, topK)
	return tree, s
}

func buildReport(root string, detail, threads int) ([]byte, *scanner) {
	t0 := time.Now()
	tree, s := scan(root, threads, 0)
	return renderHTML(root, tree, s, detail, time.Since(t0)), s
}

// renderHTML prunes the tree (in place) and embeds it into the report page.
func renderHTML(root string, tree *Node, s *scanner, detail int, elapsed time.Duration) []byte {
	threshold := tree.Size / int64(detail)
	if threshold < 1 {
		threshold = 1
	}
	prune(tree, threshold)

	type extRow [3]any
	var exts []extRow
	for k, v := range s.exts {
		exts = append(exts, extRow{k, v.bytes, v.count})
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i][1].(int64) > exts[j][1].(int64) })
	if len(exts) > 300 {
		exts = exts[:300]
	}
	data := map[string]any{
		"root":    root,
		"total":   tree.Size,
		"scanned": time.Now().Format("2006-01-02 15:04"),
		"seconds": float64(elapsed.Milliseconds()/100) / 10,
		"files":   s.files.Load(),
		"dirs":    s.dirs.Load(),
		"errors":  s.errs.Load(),
		"exts":    exts,
		"tree":    tree,
	}
	// encoding/json escapes <, > and & so the payload can't close the <script> tag.
	payload, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return []byte(strings.Replace(template, "__DATA__", string(payload), 1))
}

func serve(dir string, port int) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	url := fmt.Sprintf("http://%s/", ln.Addr())
	fmt.Printf("Serving report at %s  (Ctrl+C to stop)\n", url)
	exec.Command("open", url).Start()
	http.Serve(ln, http.FileServer(http.Dir(dir)))
}

func main() {
	port := flag.Int("port", 8765, "port to serve the report on")
	out := flag.String("out", "", "directory to write index.html into (default: temp dir)")
	detail := flag.Int("detail", 200000, "approx. max treemap tiles; smaller items are lumped")
	threads := flag.Int("threads", 16, "parallel directory scanners")
	noServe := flag.Bool("no-serve", false, "only write the report")
	asJSON := flag.Bool("json", false, "print a structured JSON analysis to stdout instead of the HTML report")
	topK := flag.Int("top", 50, "with -json: number of largest files and directories to list")
	depth := flag.Int("depth", 3, "with -json: max depth (below PATH) for the largest-directories list")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: diskmap [flags] [PATH]  (flags may also follow PATH)")
		flag.PrintDefaults()
	}
	// Allow flags before and after the path.
	path := "."
	args := os.Args[1:]
	for {
		flag.CommandLine.Parse(args)
		if flag.NArg() == 0 {
			break
		}
		path, args = flag.Arg(0), flag.Args()[1:]
	}

	root, err := filepath.Abs(path)
	if err == nil {
		if home, _ := os.UserHomeDir(); strings.HasPrefix(path, "~") {
			root = filepath.Join(home, path[1:])
		}
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "not a directory: %s\n", root)
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "Scanning %s ...\n", root)
	t0 := time.Now()
	k := 0
	if *asJSON {
		k = *topK
	}
	tree, s := scan(root, *threads, k)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(analyze(root, tree, s, *topK, *depth, time.Since(t0))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	html := renderHTML(root, tree, s, *detail, time.Since(t0))
	dir := *out
	if dir == "" {
		if dir, err = os.MkdirTemp("", "diskmap-"); err != nil {
			panic(err)
		}
	}
	dir, _ = filepath.Abs(dir)
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, "index.html")
	if err := os.WriteFile(file, html, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s (%d files, %d dirs, %d unreadable)\n", file, s.files.Load(), s.dirs.Load(), s.errs.Load())
	if !*noServe {
		serve(dir, *port)
	}
}
