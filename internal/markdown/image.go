package markdown

import (
	"bytes"
	"fmt"
	"strings"

	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type AssetResolver interface {
	ResolveAsset(reference string) string
}

type ImageEmbed struct {
	gast.BaseInline
	Reference string
}

var KindImageEmbed = gast.NewNodeKind("ImageEmbed")

func (n *ImageEmbed) Kind() gast.NodeKind       { return KindImageEmbed }
func (n *ImageEmbed) Text(source []byte) []byte { return []byte(n.Reference) }
func (n *ImageEmbed) Dump(source []byte, level int) {
	gast.DumpHelper(n, source, level, map[string]string{"Reference": n.Reference}, nil)
}

type imageEmbedParser struct{}

func (p *imageEmbedParser) Trigger() []byte { return []byte{'!'} }

func (p *imageEmbedParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	line, _ := block.PeekLine()
	if len(line) < 5 || !bytes.HasPrefix(line, []byte("![[")) {
		return nil
	}
	end := bytes.Index(line[3:], []byte("]]"))
	if end < 0 {
		return nil
	}
	reference := string(line[3 : end+3])
	if divider := strings.IndexByte(reference, '|'); divider >= 0 {
		reference = reference[:divider]
	}
	if reference == "" {
		return nil
	}
	block.Advance(end + 5)
	return &ImageEmbed{Reference: reference}
}

type imageHTMLRenderer struct{ resolver AssetResolver }

func newImageHTMLRenderer(resolver AssetResolver) renderer.NodeRenderer {
	return &imageHTMLRenderer{resolver: resolver}
}

func (r *imageHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindImageEmbed, r.renderImageEmbed)
}

func (r *imageHTMLRenderer) renderImageEmbed(w util.BufWriter, source []byte, node gast.Node, entering bool) (gast.WalkStatus, error) {
	if !entering {
		return gast.WalkContinue, nil
	}
	image := node.(*ImageEmbed)
	destination := image.Reference
	if r.resolver != nil {
		destination = r.resolver.ResolveAsset(image.Reference)
	}
	fmt.Fprintf(w, `<img src="%s" alt="%s">`, htmlEscape(destination), htmlEscape(image.Reference))
	return gast.WalkContinue, nil
}

type imageTransformer struct{ resolver AssetResolver }

func (t *imageTransformer) Transform(doc *gast.Document, reader text.Reader, pc parser.Context) {
	if t.resolver == nil {
		return
	}
	_ = gast.Walk(doc, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
		image, ok := node.(*gast.Image)
		if entering && ok {
			image.Destination = []byte(t.resolver.ResolveAsset(string(image.Destination)))
		}
		return gast.WalkContinue, nil
	})
}

type assetCollector map[string]struct{}

func (c assetCollector) ResolveAsset(reference string) string {
	c[reference] = struct{}{}
	return reference
}

func ReferencedAssets(source string) ([]string, error) {
	assets := assetCollector{}
	if _, err := RenderMarkdownWithAssets(nil, assets, source); err != nil {
		return nil, err
	}
	references := make([]string, 0, len(assets))
	for reference := range assets {
		references = append(references, reference)
	}
	return references, nil
}
