// Package markdown 提供 OSS 博客渲染用的 Goldmark 扩展：双链 [[...]] 解析。
//
// 决策 3（修正「二、2」）：
//   - 渲染 [[链接文字]] 时，在「当前用户的 shares 表」全局查找文件名
//     为「链接文字.md」的文章。仅已分享者渲染为 <a href="/p/{share_id}">。
//   - 文件夹分享覆盖其下所有文件——文件夹分享路由渲染时预先把文件夹内
//     所有文件视为"可被双链命中"，合并进全局查找集合。
//   - 同名歧义取 CreatedAt 最近更新的 share_id。
//   - 未命中渲染为 <span class="unshared-link">链接文字(未分享)</span>。
package markdown

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Wikilink 是 Obsidian 风格的 [[链接文字]] AST 节点。
type Wikilink struct {
	gast.BaseInline
	RawText string
}

func (n *Wikilink) Kind() gast.NodeKind {
	return KindWikilink
}

func (n *Wikilink) Text(source []byte) []byte {
	return []byte(n.RawText)
}

func (n *Wikilink) Dump(source []byte, level int) {
	m := map[string]string{
		"Text": n.RawText,
	}
	gast.DumpHelper(n, source, level, m, nil)
}

// KindWikilink 是 Wikilink 节点的 AST kind。
var KindWikilink = gast.NewNodeKind("Wikilink")

// --- Parser ---

type wikilinkParser struct{}

func newWikilinkParser() parser.InlineParser {
	return &wikilinkParser{}
}

func (p *wikilinkParser) Trigger() []byte {
	return []byte{'['}
}

// Parse 拦截 [[ 开头，到 ]] 结束。
// 失败时返回 nil，让 goldmark 把 [ 当普通文本。
func (p *wikilinkParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	// PeekLine 返回当前行；segment 提供偏移信息。
	line, segment := block.PeekLine()
	if len(line) < 2 || line[0] != '[' || line[1] != '[' {
		return nil
	}
	// 找到 ]] 的位置
	end := -1
	for i := 2; i < len(line); i++ {
		if line[i] == ']' && i+1 < len(line) && line[i+1] == ']' {
			end = i
			break
		}
	}
	if end < 0 {
		// 跨行双链不支持（Obsidian 也基本不跨行），失败让 [ 走普通文本
		return nil
	}
	inner := string(line[2:end])
	if inner == "" {
		return nil
	}
	// 消费整个 [[ ... ]] 区段（end 是第一个 ] 的位置，加 2 表示消费到第二个 ] 之后）
	block.Advance(end + 2)
	_ = segment
	return &Wikilink{RawText: inner}
}

// --- Renderer ---

type wikilinkHTMLRenderer struct {
	resolver LinkResolver
}

// LinkResolver 把双链文字解析为 share_id；未命中返回空。
// 决策 3：实现侧一次性加载用户 shares 索引，O(1) 查询。
type LinkResolver interface {
	// Resolve 返回 share_id（命中）或空字符串（未命中）。
	Resolve(linkText string) (shareID string)
}

func newWikilinkHTMLRenderer(resolver LinkResolver) renderer.NodeRenderer {
	return &wikilinkHTMLRenderer{resolver: resolver}
}

func (r *wikilinkHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindWikilink, r.renderWikilink)
}

func (r *wikilinkHTMLRenderer) renderWikilink(w util.BufWriter, source []byte, node gast.Node, entering bool) (gast.WalkStatus, error) {
	if !entering {
		return gast.WalkContinue, nil
	}
	n, ok := node.(*Wikilink)
	if !ok {
		return gast.WalkContinue, nil
	}
	// 决策 3：去掉别名/尺寸分隔 |（Obsidian [[X|别名]] 或 [[X|100x100]]）
	rawText := n.RawText
	linkText := rawText
	displayText := rawText
	if idx := strings.IndexByte(rawText, '|'); idx >= 0 {
		linkText = rawText[:idx]
		displayText = rawText[idx+1:]
	}

	shareID := ""
	if r.resolver != nil {
		shareID = r.resolver.Resolve(linkText)
	}
	if shareID == "" {
		fmt.Fprintf(w, `<span class="unshared-link">%s(未分享)</span>`, htmlEscape(displayText))
		return gast.WalkContinue, nil
	}
	fmt.Fprintf(w, `<a href="/p/%s" target="_blank">%s</a>`, shareID, htmlEscape(displayText))
	return gast.WalkContinue, nil
}

func htmlEscape(s string) string {
	return string(util.EscapeHTML([]byte(s)))
}

// --- Extension 装配 ---

type wikilinkExtension struct {
	resolver LinkResolver
	assets   AssetResolver
}

func (e *wikilinkExtension) Extend(m goldmark.Markdown) {
	// 始终注册 parser + renderer。resolver 为 nil 时 renderer 输出「未分享」占位。
	//
	// 注意 priority：goldmark 内置 linkParser（priority 200）的 Trigger 包含 '['，
	// 遇到 [ 总是返回 linkLabelState 节点占用字符。要拦截 [[，必须把 wikilink
	// parser 排在 linkParser 之前——priority 值更小（goldmark 按 priority 升序处理）。
	m.Parser().AddOptions(parser.WithInlineParsers(
		util.Prioritized(&imageEmbedParser{}, 50),
		util.Prioritized(newWikilinkParser(), 100),
	), parser.WithASTTransformers(
		util.Prioritized(&imageTransformer{resolver: e.assets}, 500),
	))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(newWikilinkHTMLRenderer(e.resolver), 500),
		util.Prioritized(newImageHTMLRenderer(e.assets), 500),
	))
}

// NewWikilinkExtension 创建一个双链扩展。resolver 为 nil 时所有 [[...]]
// 渲染为「未分享」占位。
func NewWikilinkExtension(resolver LinkResolver) goldmark.Extender {
	return &wikilinkExtension{resolver: resolver}
}

// NewMarkdown 构造一个带双链扩展的 goldmark 实例。
func NewMarkdown(resolver LinkResolver) goldmark.Markdown {
	return newMarkdown(resolver, nil)
}

func newMarkdown(resolver LinkResolver, assets AssetResolver) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM, &wikilinkExtension{resolver: resolver, assets: assets}),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
}

func RenderMarkdownWithAssets(resolver LinkResolver, assets AssetResolver, source string) (string, error) {
	md := newMarkdown(resolver, assets)
	var buf strings.Builder
	if err := md.Convert([]byte(source), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RenderMarkdown 便捷封装：把 markdown 文本渲染为 HTML。
func RenderMarkdown(resolver LinkResolver, source string) (string, error) {
	md := NewMarkdown(resolver)
	var buf strings.Builder
	if err := md.Convert([]byte(source), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
