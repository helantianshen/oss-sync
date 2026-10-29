package blog

import (
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/helantianshen/oss-sync/internal/markdown"
)

// 阅读时长按每分钟 400 字估算
const wordsPerMinute = 400

// ArticleMeta 是公开页面渲染时可用的文章元数据，由 Markdown frontmatter 与正文推导；
// 自定义主题通过 .ArticlePost 访问单篇文章、通过 .HomePosts 的条目访问列表数据
type ArticleMeta struct {
	Summary        string
	Date           string
	Category       string
	Tags           []string
	CoverURL       string
	WordCount      int
	ReadingMinutes int
}

// frontmatter 保存从 Markdown 头部 --- 块解析出的标量字段
type frontmatter struct {
	title       string
	description string
	published   string
	category    string
	tags        []string
	image       string
}

// splitFrontmatter 仅在文件以完整闭合的 --- 块开头时返回其中的标量与其余正文；
// 未闭合或格式损坏时原样返回输入，保证内容不丢失
func splitFrontmatter(raw string) (frontmatter, string) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return frontmatter{}, raw
	}
	end := strings.Index(normalized[4:], "\n---")
	if end < 0 {
		return frontmatter{}, raw
	}
	block := normalized[4 : 4+end]
	rest := normalized[4+end:] // 以 "\n---" 开头
	lineEnd := strings.Index(rest[1:], "\n")
	fenceLine := rest[1:]
	body := ""
	if lineEnd >= 0 {
		fenceLine = rest[1 : 1+lineEnd]
		body = rest[1+lineEnd+1:]
	}
	if strings.TrimSpace(fenceLine) != "---" {
		return frontmatter{}, raw
	}
	var metadata map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(block+"\n"), &metadata); err != nil {
		return frontmatter{}, raw
	}
	return parseFrontmatterBlock(metadata), body
}

// parseFrontmatterBlock 使用 YAML 解码后的节点，保留多行文本、引号和日期字面值
func parseFrontmatterBlock(metadata map[string]yaml.Node) frontmatter {
	fields := make(map[string]yaml.Node, len(metadata))
	for key, value := range metadata {
		fields[strings.ToLower(key)] = value
	}
	scalar := func(keys ...string) string {
		for _, key := range keys {
			if node, ok := fields[key]; ok && node.Kind == yaml.ScalarNode && node.Tag != "!!null" {
				return node.Value
			}
		}
		return ""
	}
	fm := frontmatter{
		title: scalar("title"), description: scalar("description", "summary"),
		published: scalar("published", "date"), category: scalar("category"),
		image: scalar("image", "cover"),
	}
	tags := fields["tags"]
	if tags.Kind == yaml.SequenceNode {
		for _, tag := range tags.Content {
			if tag.Kind == yaml.ScalarNode && tag.Tag != "!!null" && strings.TrimSpace(tag.Value) != "" {
				fm.tags = append(fm.tags, strings.TrimSpace(tag.Value))
			}
		}
	} else if tags.Kind == yaml.ScalarNode && tags.Tag != "!!null" {
		for _, tag := range strings.Split(tags.Value, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				fm.tags = append(fm.tags, tag)
			}
		}
	}
	return fm
}

// buildArticleMeta 汇总文章标题与元数据；标题优先 frontmatter，摘要优先 description，
// 日期优先 published，其余缺省时从正文与文件修改时间推导。resolve 把正文引用的附件路径转换为公开 URL，可为 nil
func buildArticleMeta(fm frontmatter, body, fallbackTitle string, modified time.Time, resolve func(string) string) (string, ArticleMeta) {
	title := strings.TrimSpace(fm.title)
	if title == "" {
		title = basenameNoExt(fallbackTitle)
	}
	meta := ArticleMeta{
		Summary:  strings.TrimSpace(fm.description),
		Date:     parsePostDate(fm.published, modified),
		Category: fm.category,
		Tags:     fm.tags,
	}
	if meta.Summary == "" {
		meta.Summary = firstBodyLine(body)
	}
	meta.WordCount = countWords(body)
	meta.ReadingMinutes = (meta.WordCount + wordsPerMinute - 1) / wordsPerMinute
	if fm.image != "" {
		switch {
		case strings.HasPrefix(fm.image, "http://"), strings.HasPrefix(fm.image, "https://"), strings.HasPrefix(fm.image, "/"):
			meta.CoverURL = fm.image
		case resolve != nil:
			meta.CoverURL = resolve(fm.image)
		}
	}
	return title, meta
}

// firstBodyLine 从正文提取最多 120 字的纯文本摘要。
func firstBodyLine(body string) string {
	return markdown.Summary(body, 120)
}

// parsePostDate 优先使用 frontmatter 的发布日期，支持常见日期格式，缺省回退文件修改时间
func parsePostDate(published string, modified time.Time) string {
	if published != "" {
		for _, layout := range []string{"2006-01-02", "2006-01-02 15:04", "2006-01-02 15:04:05", time.RFC3339, "2006/01/02"} {
			if t, err := time.Parse(layout, published); err == nil {
				return t.Format("2006-01-02")
			}
		}
		return published
	}
	if !modified.IsZero() {
		return modified.Format("2006-01-02")
	}
	return ""
}

// countWords 统计正文字数：中日韩字符按字计，其余连续字母数字按词计
func countWords(body string) int {
	count := 0
	inWord := false
	for _, r := range body {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			count++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if !inWord {
				count++
				inWord = true
			}
		default:
			inWord = false
		}
	}
	return count
}
