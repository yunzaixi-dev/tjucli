package librarysync

import (
	"encoding/json"
	"strconv"
	"strings"
)

type richNode struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Attrs struct {
		Level int `json:"level"`
	} `json:"attrs"`
	Content []richNode `json:"content"`
}

// RichTextToMarkdown renders the editor's JSON document as readable Markdown:
// headings, lists, quotes, code blocks and paragraphs. It matches the web
// app's export, so a cloned rich-text note reads the same as a downloaded one.
func RichTextToMarkdown(body string) string {
	var doc richNode
	if json.Unmarshal([]byte(body), &doc) != nil {
		return body
	}
	var text func(n richNode) string
	text = func(n richNode) string {
		if n.Text != "" || len(n.Content) == 0 {
			return n.Text
		}
		var b strings.Builder
		for _, c := range n.Content {
			b.WriteString(text(c))
		}
		return b.String()
	}
	var block func(n richNode, depth int) string
	list := func(n richNode, depth int, marker func(int) string) string {
		var lines []string
		for i, item := range n.Content {
			var parts []string
			for _, child := range item.Content {
				parts = append(parts, block(child, depth+1))
			}
			lines = append(lines, strings.Repeat("  ", depth)+marker(i)+strings.TrimLeft(strings.Join(parts, "\n"), " "))
		}
		return strings.Join(lines, "\n")
	}
	block = func(n richNode, depth int) string {
		switch n.Type {
		case "heading":
			level := min(max(n.Attrs.Level, 1), 6)
			return strings.Repeat("#", level) + " " + text(n)
		case "bulletList":
			return list(n, depth, func(int) string { return "- " })
		case "orderedList":
			return list(n, depth, func(i int) string { return strconv.Itoa(i+1) + ". " })
		case "blockquote":
			var lines []string
			for _, c := range n.Content {
				lines = append(lines, "> "+block(c, depth))
			}
			return strings.Join(lines, "\n")
		case "codeBlock":
			return "```\n" + text(n) + "\n```"
		case "horizontalRule":
			return "---"
		}
		nested := false
		for _, c := range n.Content {
			if len(c.Content) > 0 {
				nested = true
				break
			}
		}
		if !nested {
			return text(n)
		}
		var parts []string
		for _, c := range n.Content {
			parts = append(parts, block(c, depth))
		}
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(block(doc, 0)) + "\n"
}
