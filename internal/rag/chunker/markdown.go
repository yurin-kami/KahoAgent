package chunker

import (
	"context"
	"regexp"
	"strings"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// MetaHeadingPath 是存放标题路径的 metadata key。
//
// 检索命中后可据此告诉用户「这段来自 《Redis笔记》> 高可用 > 主从复制」，
// 这是答案溯源功能的基础。Phase 3 的 rag_query tool 会读取它。
const MetaHeadingPath = "heading_path"

// headingSep 是标题路径的层级分隔符。
const headingSep = " > "

// atxHeading 匹配 Markdown ATX 标题：# 到 ###### 后跟空格和内容。
//
// 只支持 ATX 而不支持 Setext（下划线式标题），因为后者在技术文档中罕见，
// 且需要前瞻一行才能识别，会让扫描逻辑复杂化。
var atxHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

// fenceMarker 匹配代码围栏：``` 或 ~~~ 开头。
var fenceMarker = regexp.MustCompile("^\\s*(```|~~~)")

// MarkdownChunker 按标题层级切分文档。
//
// 设计动机：技术文档的标题天然是语义边界。「## Redis 主从复制」下的内容
// 一定都在讲主从复制，按标题切能让块内语义高度聚焦，
// 检索准确率显著高于按固定长度硬切。
//
// 超长小节的处理：若某标题下内容超过 maxSize，退化为调用 PlainChunker
// 二次切分，但保留 heading_path —— 复用而非重写，这正是接口设计的价值。
type MarkdownChunker struct {
	maxSize  int
	fallback *PlainChunker
}

var _ rag.Chunker = (*MarkdownChunker)(nil)

// NewMarkdownChunker 创建 Markdown 分块器。
// maxSize 是单块字符上限，超出会触发二次切分。
func NewMarkdownChunker(maxSize int) *MarkdownChunker {
	if maxSize <= 0 {
		maxSize = DefaultSize * 2 // 标题块允许比固定块大一些
	}
	return &MarkdownChunker{
		maxSize:  maxSize,
		fallback: NewPlainChunker(maxSize, maxSize/6),
	}
}

func (c *MarkdownChunker) Name() string { return "markdown" }

// section 是扫描阶段的中间产物：一个标题及其下属内容。
type section struct {
	headingPath string
	body        string
}

func (c *MarkdownChunker) Chunk(ctx context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
	sections := c.scan(string(raw))

	// 分两阶段：先扫描出 section，再统一编号。
	// 这样二次切分产生的额外块也能拿到连续的全局 Index，
	// 而不会出现 0,1,2 后又跳回 0 的情况。
	var chunks []rag.Chunk
	idx := 0

	for _, sec := range sections {
		body := strings.TrimSpace(sec.body)
		if body == "" {
			continue // 只有标题没有正文的节直接跳过，入库也检索不出东西
		}

		if len([]rune(body)) <= c.maxSize {
			chunks = append(chunks, c.newChunk(docID, idx, body, sec.headingPath))
			idx++
			continue
		}

		// 超长节：复用 PlainChunker 二次切分，但保留 heading_path。
		// 传空 docID 是因为这里只借用它的切分逻辑，ID 由外层统一生成。
		parts, err := c.fallback.Chunk(ctx, "", []byte(body))
		if err != nil {
			return nil, err
		}
		for _, p := range parts {
			chunks = append(chunks, c.newChunk(docID, idx, p.Content, sec.headingPath))
			idx++
		}
	}
	return chunks, nil
}

func (c *MarkdownChunker) newChunk(docID string, idx int, content, headingPath string) rag.Chunk {
	return rag.Chunk{
		ID:      ChunkID(docID, idx),
		DocID:   docID,
		Index:   idx,
		Content: content,
		Metadata: map[string]string{
			"chunker":       c.Name(),
			MetaHeadingPath: headingPath,
		},
	}
}

// scan 逐行扫描，按标题切出 section 并维护标题路径。
func (c *MarkdownChunker) scan(text string) []section {
	var (
		sections  []section
		stack     []string // 标题栈，index 0 对应一级标题
		body      strings.Builder
		inFence   bool
		fenceKind string
	)

	flush := func() {
		if body.Len() > 0 {
			sections = append(sections, section{
				headingPath: strings.Join(stack, headingSep),
				body:        body.String(),
			})
			body.Reset()
		}
	}

	for _, line := range strings.Split(text, "\n") {
		// ⚠️ 代码块内的 # 是注释不是标题。不跟踪围栏状态的话，
		//    一段 Go 代码里的 "# TODO" 会被误判成标题把块切碎。
		if m := fenceMarker.FindStringSubmatch(line); m != nil {
			if !inFence {
				inFence, fenceKind = true, m[1]
			} else if m[1] == fenceKind {
				// 只有同类围栏才能闭合：``` 不能关掉 ~~~
				inFence, fenceKind = false, ""
			}
			body.WriteString(line)
			body.WriteByte('\n')
			continue
		}

		if !inFence {
			if m := atxHeading.FindStringSubmatch(line); m != nil {
				flush() // 新标题前先把已累积的内容结算掉

				level := len(m[1])
				title := m[2]

				// 截断到父层再压入当前标题，实现层级回退。
				// 例：栈为 [A, A1]，遇到一级标题 B 时截断到 0 层，
				// 得到 [B] 而非 [A, A1, B]。
				if level-1 < len(stack) {
					stack = stack[:level-1]
				}
				// 层级跳跃（如 # 直接到 ###）时补空占位，
				// 保证 stack 长度与 level 对齐。
				for len(stack) < level-1 {
					stack = append(stack, "")
				}
				stack = append(stack, title)
				continue
			}
		}

		body.WriteString(line)
		body.WriteByte('\n')
	}

	flush()
	return sections
}
