package markdown

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummarySkipsNavigationAndNonProse(t *testing.T) {
	source := "### 目录\n\n[[#基础使用|开始使用]]\n\n[参数设置](#settings)\n\n[TOC]\n\n" +
		"![界面截图](https://example.com/screen.webp)\n\n![[screen.png]]\n\n" +
		"```go\nfmt.Println(\"代码示例\")\n```\n\n" +
		"| 项目 | 值 |\n| --- | --- |\n| 示例 | 数据 |\n\n" +
		"# 基础使用\n\n- 点击==`绘制路线`==按钮，进入**路线界面**。\n- 参考[官方文档](https://example.com/docs)设置参数。"
	want := "点击绘制路线按钮，进入路线界面。 参考官方文档设置参数。"
	if got := Summary(source, 120); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSummaryLinkAndFormattingSemantics(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"ordinary link", "[官方文档](https://example.com/docs)", "官方文档"},
		{"ordinary wikilink", "[[使用指南|操作说明]]", "操作说明"},
		{"anchor navigation", "[[#操作步骤]]\n\n[常见问题](#faq)", ""},
		{"anchor in prose", "请参考[参数设置](#settings)完成配置。", "请参考参数设置完成配置。"},
		{"wikilink in prose", "请参考 [[使用指南|操作说明]] 完成配置。", "请参考 操作说明 完成配置。"},
		{"formatting", "使用 **加粗**、*强调*、~~删除线~~ 与 `命令`。", "使用 加粗、强调、删除线 与 命令。"},
		{"entities", "A &amp; B 使用 \\*普通星号\\*。", "A & B 使用 *普通星号*。"},
		{"inline code only", "`example --help`", ""},
		{"image only", "![说明](picture.png)\n\n![[picture.png]]", ""},
		{"raw URL only", "https://example.com/a/very/long/path", ""},
		{"hidden HTML", "正常 <script>隐藏内容</script><style>隐藏样式</style> 介绍。", "正常 介绍。"},
		{"HTML block", "<script>\n隐藏内容\n</script>\n\n实际介绍。", "实际介绍。"},
		{"soft line breaks", "第一行介绍，\n第二行说明。", "第一行介绍， 第二行说明。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summary(tc.source, 120); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummaryUnicodeLimit(t *testing.T) {
	got := Summary(strings.Repeat("中文😀", 60), 120)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 121 || !strings.HasSuffix(got, "…") {
		t.Fatalf("invalid or unbounded summary: %q", got)
	}
	if got := Summary("正文", 0); got != "" {
		t.Fatalf("zero limit returned %q", got)
	}
}
