package librarysync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutNamesNotesFoldersAndCollisions(t *testing.T) {
	l := buildLayout([]Entry{
		{ID: "f1", Kind: KindFolder, Title: "课程/2026"},
		{ID: "n1", Kind: KindNote, Title: "笔记", ParentID: "f1"},
		{ID: "n2", Kind: KindNote, Title: "笔记", ParentID: "f1", SortOrder: 1},
		{ID: "c1", Kind: KindNote, Title: "子笔记", ParentID: "n1"},
		{ID: "x1", Kind: KindFile, Title: "成绩.xlsx", ParentID: "f1"},
		{ID: "a1", Kind: "agent", Title: "新手向导"},
		{ID: "o1", Kind: KindNote, Title: "孤儿", ParentID: "missing"},
	})
	want := map[string]string{
		"n1": "课程_2026/笔记.md", "n2": "课程_2026/笔记 (2).md", "c1": "课程_2026/笔记/子笔记.md",
		"x1": "课程_2026/成绩.xlsx", "o1": "孤儿.md",
	}
	for id, p := range want {
		if l.paths[id] != p {
			t.Errorf("%s: %q, want %q", id, l.paths[id], p)
		}
	}
	if _, ok := l.paths["a1"]; ok {
		t.Error("an agent entry without children is not a file")
	}
	if l.dirs["课程_2026/笔记"] != "n1" {
		t.Error("a note's children live in a directory of its name")
	}
}

func TestStatusFindsEditsMovesAndUnpushable(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "same")
	write("b.md", "edited")
	write("moved/c.txt", "file bytes")
	write("rich.md", "changed export")
	write("new.md", "new note")
	write(".hidden", "ignored")
	write("big.bin", strings.Repeat("x", maxFileBytes+1))
	c := &Copy{Root: root, State: State{Entries: map[string]Tracked{
		"a.md":    {ID: "a", Kind: KindNote, SHA: digest([]byte("same"))},
		"b.md":    {ID: "b", Kind: KindNote, SHA: digest([]byte("before"))},
		"c.txt":   {ID: "c", Kind: KindFile, SHA: digest([]byte("file bytes"))},
		"rich.md": {ID: "r", Kind: KindRichText, SHA: digest([]byte("export"))},
		"gone.md": {ID: "g", Kind: KindNote, SHA: digest([]byte("x"))},
	}}}
	changes, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, ch := range changes {
		got[ch.Op+" "+ch.Path] = ch
	}
	for key, note := range map[string]string{
		"modified b.md": "", "moved moved/c.txt": "", "modified rich.md": "rich_text_read_only",
		"added new.md": "", "deleted gone.md": "", "added moved": "", "added big.bin": "too_large",
	} {
		ch, ok := got[key]
		if !ok || ch.Note != note {
			t.Errorf("%s: %+v (found %v)", key, ch, ok)
		}
	}
	if got["moved moved/c.txt"].From != "c.txt" {
		t.Error("move source")
	}
	for key := range got {
		if strings.Contains(key, "a.md") || strings.Contains(key, "hidden") {
			t.Errorf("unexpected %s", key)
		}
	}
}

func TestRichTextToMarkdown(t *testing.T) {
	doc := `{"type":"doc","content":[
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"计划"}]},
		{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"复习"}]}]}]},
		{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"一"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"二"}]}]}]},
		{"type":"codeBlock","content":[{"type":"text","text":"go test"}]}]}`
	got := RichTextToMarkdown(doc)
	for _, want := range []string{"## 计划", "- 复习", "1. 一\n2. 二", "```\ngo test\n```"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if RichTextToMarkdown("not json") != "not json" {
		t.Error("non-JSON body passes through")
	}
}

func TestUnifiedDiff(t *testing.T) {
	d := UnifiedDiff("a\nb\nc\n", "a\nB\nc\nd\n", "a/x.md", "b/x.md")
	want := "--- a/x.md\n+++ b/x.md\n@@ -1,3 +1,4 @@\n a\n-b\n+B\n c\n+d\n"
	if d != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", d, want)
	}
	if !strings.Contains(UnifiedDiff(strings.Repeat("x\n", maxDiffLines+1), "", "a", "b"), "省略") {
		t.Fatal("huge files are summarised")
	}
}
