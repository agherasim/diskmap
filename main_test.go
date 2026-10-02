package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func allocSize(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func TestExtOf(t *testing.T) {
	cases := map[string]string{
		"a.txt": ".txt", "B.TXT": ".txt", "archive.tar.gz": ".gz",
		".bashrc": "(none)", "..foo": "(none)", ".hidden.json": ".json",
		"Makefile": "(none)", "file.": ".", "db.sqlite-wal": ".sqlite-wal", "x.c++": ".c++",
		"cache.d483549aedc03483b244042288d43c71f313d811a41138221e8da39feefd188a": "(none)",
		"blob.0~3kgbezt2lb6vvacsjvwhz":                                           "(none)", "v1.2 (copy)": "(none)",
	}
	for name, want := range cases {
		if got := extOf(name); got != want {
			t.Errorf("extOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// The getattrlistbulk reader must agree field by field with readdir+lstat.
func TestListDirMatchesStat(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "small.txt"), 10)
	writeFile(t, filepath.Join(dir, "big.bin"), 100_000)
	writeFile(t, filepath.Join(dir, ".dotfile"), 1)
	writeFile(t, filepath.Join(dir, "ünïcødé name.md"), 5000)
	writeFile(t, filepath.Join(dir, "sub", "x"), 1)
	if err := os.Link(filepath.Join(dir, "big.bin"), filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("small.txt", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}

	bulk, err := listDir(dir, make([]byte, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := listDirStat(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := func(es []entry) { sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name }) }
	byName(bulk)
	byName(stat)
	if len(bulk) != 7 || len(bulk) != len(stat) {
		t.Fatalf("got %d bulk / %d stat entries, want 7", len(bulk), len(stat))
	}
	for i := range bulk {
		if bulk[i] != stat[i] {
			t.Errorf("entry mismatch:\n bulk %+v\n stat %+v", bulk[i], stat[i])
		}
	}
}

// A tiny buffer forces several getattrlistbulk calls per directory.
func TestListDirSmallBuffer(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("file-%03d.dat", i)), 1)
	}
	ents, err := listDir(dir, make([]byte, 1024))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 300 {
		t.Fatalf("got %d entries, want 300", len(ents))
	}
}

func TestScanRollupAndHardlinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 5000)
	writeFile(t, filepath.Join(root, ".bashrc"), 300)
	writeFile(t, filepath.Join(root, "sub", "b.TXT"), 20_000)
	writeFile(t, filepath.Join(root, "sub", "deeper", "c.go"), 70_000)
	if err := os.Link(filepath.Join(root, "sub", "b.TXT"), filepath.Join(root, "sub", "deeper", "b-link")); err != nil {
		t.Fatal(err)
	}

	tree, s := scan(root, 4, 0)

	var want int64
	for _, p := range []string{"a.txt", ".bashrc", "sub/b.TXT", "sub/deeper/c.go"} {
		want += allocSize(t, filepath.Join(root, p))
	}
	if tree.Size != want {
		t.Errorf("total = %d, want %d (hard link must count once)", tree.Size, want)
	}
	if tree.Files != 5 || s.files.Load() != 5 {
		t.Errorf("files = %d / %d, want 5", tree.Files, s.files.Load())
	}
	if d := s.dirs.Load(); d != 3 {
		t.Errorf("dirs = %d, want 3", d)
	}
	if c := s.exts[".txt"].count; c != 2 {
		t.Errorf(".txt count = %d, want 2", c)
	}
	if c := s.exts["(none)"].count; c != 2 {
		t.Errorf("(none) count = %d, want 2 (.bashrc, b-link)", c)
	}

	// every directory's size is the sum of its children
	var check func(n *Node)
	check = func(n *Node) {
		if n.Children == nil {
			return
		}
		var sum int64
		for _, c := range n.Children {
			sum += c.Size
			check(c)
		}
		if sum != n.Size {
			t.Errorf("%s: size %d != sum of children %d", n.Name, n.Size, sum)
		}
	}
	check(tree)
}

func TestScanLumpsManyFiles(t *testing.T) {
	root := t.TempDir()
	n := keepFilesPerDir + 50
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%04d.log", i)), 4096*(1+i%3))
	}
	tree, _ := scan(root, 2, 0)
	if tree.Files != int64(n) {
		t.Errorf("files = %d, want %d", tree.Files, n)
	}
	if len(tree.Children) != keepFilesPerDir+1 {
		t.Fatalf("children = %d, want %d", len(tree.Children), keepFilesPerDir+1)
	}
	var lump *Node
	for _, c := range tree.Children {
		if c.Ext == "*" {
			lump = c
		}
	}
	if lump == nil || lump.Name != "<50 smaller files>" {
		t.Fatalf("missing lump node, got %+v", lump)
	}
}

func TestPrune(t *testing.T) {
	tree := &Node{Name: "root", Size: 1000, Children: []*Node{
		{Name: "small1", Size: 5},
		{Name: "big", Size: 900, Children: []*Node{{Name: "inner", Size: 900}}},
		{Name: "empty", Size: 0},
		{Name: "small2", Size: 7},
		{Name: "mid", Size: 88},
	}}
	prune(tree, 10)
	var got []string
	for _, c := range tree.Children {
		got = append(got, fmt.Sprintf("%s=%d", c.Name, c.Size))
	}
	want := "big=900 mid=88 <2 small items>=12"
	if strings.Join(got, " ") != want {
		t.Errorf("pruned children = %v, want %s", got, want)
	}
}

func TestBuildReport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "</script><b>.txt"), 9000)
	writeFile(t, filepath.Join(root, "dir", "x.png"), 50_000)

	html, _ := buildReport(root, 1000, 2)
	page := string(html)
	if strings.Contains(page, "__DATA__") {
		t.Fatal("template placeholder was not replaced")
	}
	start := strings.Index(page, "const D = ")
	end := strings.Index(page[start:], ";\n")
	payload := page[start+len("const D = ") : start+end]
	if strings.Contains(payload, "</") {
		t.Error("payload contains '</', which could close the <script> tag")
	}
	var data struct {
		Root  string  `json:"root"`
		Total int64   `json:"total"`
		Files int64   `json:"files"`
		Exts  [][]any `json:"exts"`
		Tree  Node    `json:"tree"`
	}
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if data.Root != root || data.Files != 2 || data.Total != data.Tree.Size || data.Total == 0 {
		t.Errorf("unexpected report header: root=%s files=%d total=%d tree=%d", data.Root, data.Files, data.Total, data.Tree.Size)
	}
	if len(data.Exts) != 2 {
		t.Errorf("exts = %v, want 2 rows", data.Exts)
	}
}

func TestTopFiles(t *testing.T) {
	root := t.TempDir()
	sizes := map[string]int{"a/big.iso": 900_000, "b/c/mid.mov": 500_000, "small.txt": 10, "b/d.zip": 700_000, "e.log": 100_000}
	for p, n := range sizes {
		writeFile(t, filepath.Join(root, p), n)
	}
	_, s := scan(root, 4, 3)
	var got []string
	for _, f := range s.top {
		got = append(got, strings.TrimPrefix(f.path, root+"/"))
	}
	if want := "a/big.iso b/d.zip b/c/mid.mov"; strings.Join(got, " ") != want {
		t.Errorf("top files = %v, want %s", got, want)
	}
}

func TestAnalyze(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "proj", "node_modules", "pkg", "index.js"), 300_000)
	writeFile(t, filepath.Join(root, "proj", "node_modules", "pkg", "node_modules", "dep", "x.js"), 200_000)
	writeFile(t, filepath.Join(root, "proj", "src", "main.ts"), 5_000)
	writeFile(t, filepath.Join(root, "py", ".venv", "lib", "site.py"), 100_000)
	writeFile(t, filepath.Join(root, "old.dmg"), 400_000)
	old := time.Now().AddDate(-2, 0, 0)
	if err := os.Chtimes(filepath.Join(root, "old.dmg"), old, old); err != nil {
		t.Fatal(err)
	}

	tree, s := scan(root, 4, 10)
	r := analyze(root, tree, s, 10, 2, 0)

	if r.TotalBytes != tree.Size || r.Files != 5 || r.Volume == nil || r.Volume.FreeBytes <= 0 {
		t.Errorf("bad header: %+v", r)
	}
	if len(r.TopFiles) != 5 || !strings.HasSuffix(r.TopFiles[0].Path, "old.dmg") || r.TopFiles[0].AgeDays < 700 {
		t.Errorf("top files = %+v", r.TopFiles)
	}
	// nested node_modules is inside the outer one: reported once
	var kinds []string
	for _, k := range r.KnownTotals {
		kinds = append(kinds, fmt.Sprintf("%s:%d", k.Kind, k.Count))
	}
	if want := "npm/pnpm/yarn dependencies:1 Python virtualenv:1"; strings.Join(kinds, " ") != want {
		t.Errorf("regenerable totals = %v, want %s", kinds, want)
	}
	if len(r.KnownDirs) != 2 || !strings.HasSuffix(r.KnownDirs[0].Path, "proj/node_modules") {
		t.Errorf("regenerable dirs = %+v", r.KnownDirs)
	}
	for _, d := range r.TopDirs {
		if d.Depth > 2 {
			t.Errorf("top dir deeper than -depth 2: %+v", d)
		}
	}
	if r.TopDirs[0].Path != filepath.Join(root, "proj") {
		t.Errorf("largest dir = %s, want proj", r.TopDirs[0].Path)
	}
}
