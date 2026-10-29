package markdown

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Summary 从正文提取纯文本摘要，忽略目录、图片、代码块和表格。
func Summary(source string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	raw := []byte(source)
	doc := NewMarkdown(nil).Parser().Parse(text.NewReader(raw))
	var paragraphs []string
	length := 0
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.Kind() {
		case ast.KindHeading, ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindHTMLBlock, extast.KindTable:
			return ast.WalkSkipChildren, nil
		case ast.KindParagraph, ast.KindTextBlock:
			value, prose := summaryParagraph(node, raw)
			if !prose || isSummaryPlaceholder(value) {
				return ast.WalkSkipChildren, nil
			}
			paragraphs = append(paragraphs, value)
			length += utf8.RuneCountInString(value) + 1
			if length > maxRunes {
				return ast.WalkStop, nil
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	value := strings.Join(paragraphs, " ")
	if utf8.RuneCountInString(value) > maxRunes {
		value = strings.TrimSpace(string([]rune(value)[:maxRunes])) + "…"
	}
	return value
}

func isSummaryPlaceholder(value string) bool {
	switch strings.ToLower(value) {
	case "", "目录", "目 录", "toc", "contents", "table of contents", "[toc]", "{:toc}":
		return true
	}
	return false
}

// summaryParagraph 保留普通链接文字与行内代码，纯章节导航不充当摘要。
func summaryParagraph(block ast.Node, source []byte) (string, bool) {
	var result strings.Builder
	prose := false
	navigationDepth, hiddenDepth := 0, 0
	write := func(value string, countsAsProse bool) {
		result.WriteString(value)
		if countsAsProse && strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
			prose = true
		}
	}
	_ = ast.Walk(block, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := node.(*ast.Link); ok && strings.HasPrefix(strings.TrimSpace(string(link.Destination)), "#") {
			if entering {
				navigationDepth++
			} else {
				navigationDepth--
			}
		}
		if !entering {
			return ast.WalkContinue, nil
		}
		if tag, ok := node.(*ast.RawHTML); ok {
			raw := strings.ToLower(strings.TrimSpace(string(tag.Segments.Value(source))))
			closing := strings.HasPrefix(raw, "</")
			name := strings.TrimLeft(raw, "</")
			if end := strings.IndexAny(name, " \t\r\n/>"); end >= 0 {
				name = name[:end]
			}
			switch name {
			case "script", "style", "template", "noscript":
				if closing && hiddenDepth > 0 {
					hiddenDepth--
				} else if !closing && !strings.HasSuffix(raw, "/>") {
					hiddenDepth++
				}
			}
			return ast.WalkSkipChildren, nil
		}
		if hiddenDepth > 0 {
			return ast.WalkSkipChildren, nil
		}
		switch node := node.(type) {
		case *ast.Image, *ImageEmbed, *ast.AutoLink:
			return ast.WalkSkipChildren, nil
		case *Wikilink:
			label := node.RawText
			target, alias, found := strings.Cut(label, "|")
			if found {
				label = alias
			}
			write(strings.TrimSpace(strings.TrimPrefix(label, "#")), navigationDepth == 0 && !strings.HasPrefix(strings.TrimSpace(target), "#"))
			return ast.WalkSkipChildren, nil
		case *highlight:
			value := node.Raw
			code := strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`") && len(value) > 2
			if code {
				value = value[1 : len(value)-1]
			}
			write(value, navigationDepth == 0 && !code)
		case *ast.Text:
			value := node.Segment.Value(source)
			code := node.Parent() != nil && node.Parent().Kind() == ast.KindCodeSpan
			if code {
				write(string(value), false)
			} else {
				write(html.UnescapeString(string(util.UnescapePunctuations(value))), navigationDepth == 0)
			}
			if node.SoftLineBreak() || node.HardLineBreak() {
				write(" ", false)
			}
		case *ast.String:
			write(html.UnescapeString(string(node.Value)), navigationDepth == 0)
		}
		return ast.WalkContinue, nil
	})
	return strings.Join(strings.Fields(result.String()), " "), prose
}
