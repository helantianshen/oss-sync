package markdown

import (
	"fmt"
	"testing"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type debugNode struct {
	gast.BaseInline
	Raw string
}

var kindDebug = gast.NewNodeKind("Debug")

func (n *debugNode) Kind() gast.NodeKind { return kindDebug }
func (n *debugNode) Text(source []byte) []byte { return []byte(n.Raw) }
func (n *debugNode) Dump(source []byte, level int) {}

type debugParser struct{}

func (p *debugParser) Trigger() []byte { return []byte{'['} }
func (p *debugParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	line, _ := block.PeekLine()
	fmt.Printf("DEBUG Parse called: line=%q\n", string(line))
	if len(line) < 2 || line[0] != '[' || line[1] != '[' {
		return nil
	}
	// find ]]
	end := -1
	for i := 2; i < len(line); i++ {
		if line[i] == ']' && i+1 < len(line) && line[i+1] == ']' {
			end = i
			break
		}
	}
	if end < 0 {
		return nil
	}
	inner := string(line[2:end])
	block.Advance(end + 2)
	fmt.Printf("DEBUG consumed: inner=%q\n", inner)
	return &debugNode{Raw: inner}
}

type debugRenderer struct{}

func (r *debugRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindDebug, func(w util.BufWriter, source []byte, node gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		n := node.(*debugNode)
		fmt.Fprintf(w, "[[DEBUG:%s]]", n.Raw)
		return gast.WalkContinue, nil
	})
}

func TestDebugWikilink(t *testing.T) {
	md := goldmark.New(
		goldmark.WithExtensions(
			&debugExt{},
		),
	)
	var buf struct{ s string }
	_ = buf
	src := `hello [[world]] end`
	var out []byte
	w := &byteWriter{&out}
	if err := md.Convert([]byte(src), w); err != nil {
		t.Fatalf("convert: %v", err)
	}
	t.Logf("output: %s", string(out))
}

type debugExt struct{}

func (e *debugExt) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(
		util.Prioritized(&debugParser{}, 500),
	))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(&debugRenderer{}, 500),
	))
}

type byteWriter struct{ b *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.b = append(*w.b, p...)
	return len(p), nil
}
func (w *byteWriter) WriteString(s string) (int, error) {
	*w.b = append(*w.b, s...)
	return len(s), nil
}
func (w *byteWriter) WriteByte(b byte) error {
	*w.b = append(*w.b, b)
	return nil
}
func (w *byteWriter) WriteRune(r rune) (int, error) {
	return w.WriteString(string(r))
}
