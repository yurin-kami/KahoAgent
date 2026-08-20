package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/pkg/cloudkaho"
)

// FileClient 是文件工具依赖的云盘能力。
//
// 抽成接口而非直接依赖 *cloudkaho.Client：工具测试不该需要
// 起一个真的云盘服务。
type FileClient interface {
	Search(ctx context.Context, keyword string, limit int) ([]cloudkaho.FileDetail, error)
	Metadata(ctx context.Context, fileID uint) (cloudkaho.FileDetail, error)
	Share(ctx context.Context, fileID uint, expireHours int) (string, error)
}

const (
	defaultFileLimit = 10
	maxFileLimit     = 50

	defaultExpireHours = 24
	// maxExpireHours 是 AWS 预签名 URL 的硬上限（7 天）
	maxExpireHours = 7 * 24
)

// ⚠️ 与 rag_query 同样的约定：所有工具**从不返回 error**。
//
// eino 在 compose/tool_node.go:1203 对任何非中断类的工具错误
// 都会中止整个节点 —— 一次云盘抖动就会让整个对话崩掉。
// 把失败变成模型能读到的文本，模型才有机会纠错或如实告知用户。

// ---- file_search ----

type FileSearchTool struct {
	client FileClient
	logger *slog.Logger
}

var _ tool.InvokableTool = (*FileSearchTool)(nil)

func NewFileSearchTool(c FileClient, logger *slog.Logger) (*FileSearchTool, error) {
	if c == nil {
		return nil, fmt.Errorf("file client must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FileSearchTool{client: c, logger: logger}, nil
}

func (t *FileSearchTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "file_search",
		// 与 rag_query 的边界要写清楚，否则模型会混用：
		// 找「文件」用这个，找「文件里讲了什么」用 rag_query
		Desc: "按文件名搜索用户云盘中的文件，返回文件列表及其 ID、大小。" +
			"当用户想找某个文件、列出文件、或需要文件 ID 用于后续操作（如分享）时使用。" +
			"注意：本工具只匹配文件名，不搜索文件内容 —— " +
			"要根据文档内容回答问题请用 rag_query。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"keyword": {
				Type: schema.String,
				Desc: "文件名关键词。留空则返回最近的文件",
			},
			"limit": {
				Type: schema.Integer,
				Desc: fmt.Sprintf("返回数量，默认 %d，最大 %d", defaultFileLimit, maxFileLimit),
			},
		}),
	}, nil
}

func (t *FileSearchTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		Keyword string `json:"keyword"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.logger.Warn("could not parse file_search arguments", "raw", truncate(argsJSON, 200), "err", err)
		return fmt.Sprintf("参数解析失败：%s。keyword 为字符串，limit 为整数。",
			truncate(err.Error(), 150)), nil
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultFileLimit
	}
	if limit > maxFileLimit {
		limit = maxFileLimit
	}

	files, err := t.client.Search(ctx, strings.TrimSpace(args.Keyword), limit)
	if err != nil {
		t.logger.Warn("file search failed", "keyword", args.Keyword, "err", err)
		return fmt.Sprintf("文件搜索失败（%s）。请如实告知用户当前无法访问云盘，不要编造文件列表。",
			truncate(err.Error(), 200)), nil
	}
	if len(files) == 0 {
		if args.Keyword == "" {
			return "云盘中没有文件。", nil
		}
		return fmt.Sprintf("没有找到文件名包含「%s」的文件。", args.Keyword), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "找到 %d 个文件：\n", len(files))
	for i, f := range files {
		// 带上 file_id —— 后续的 file_metadata / file_share 都要用它。
		// 不给的话模型只能瞎猜 ID，或者再搜一次。
		fmt.Fprintf(&sb, "%d. %s（file_id=%d，大小 %s，上传于 %s）\n",
			i+1, f.FileName, f.FileID, humanSize(f.FileSize), humanTime(f.CreatedAt))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// ---- file_metadata ----

type FileMetadataTool struct {
	client FileClient
	logger *slog.Logger
}

var _ tool.InvokableTool = (*FileMetadataTool)(nil)

func NewFileMetadataTool(c FileClient, logger *slog.Logger) (*FileMetadataTool, error) {
	if c == nil {
		return nil, fmt.Errorf("file client must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FileMetadataTool{client: c, logger: logger}, nil
}

func (t *FileMetadataTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "file_metadata",
		Desc: "查询单个文件的详细信息，包括文件大小、上传时间、文件名。" +
			"当用户询问某个具体文件的属性（多大、什么时候传的）时使用。" +
			"需要先通过 file_search 获得 file_id。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"file_id": {
				Type:     schema.Integer,
				Desc:     "文件 ID，可从 file_search 的结果中获得",
				Required: true,
			},
		}),
	}, nil
}

func (t *FileMetadataTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		FileID uint `json:"file_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.logger.Warn("could not parse file_metadata arguments", "raw", truncate(argsJSON, 200), "err", err)
		return fmt.Sprintf("参数解析失败：%s。file_id 为正整数。",
			truncate(err.Error(), 150)), nil
	}
	if args.FileID == 0 {
		return "参数 file_id 不能为空或 0。请先用 file_search 查到文件 ID。", nil
	}

	f, err := t.client.Metadata(ctx, args.FileID)
	if err != nil {
		t.logger.Warn("file metadata lookup failed", "file_id", args.FileID, "err", err)
		return fmt.Sprintf("查询文件信息失败（%s）。请如实告知用户，不要编造文件属性。",
			truncate(err.Error(), 200)), nil
	}

	return fmt.Sprintf("文件名：%s\n文件 ID：%d\n大小：%s（%d 字节）\n上传时间：%s",
		f.FileName, f.FileID, humanSize(f.FileSize), f.FileSize, humanTime(f.CreatedAt)), nil
}

// ---- file_share ----

type FileShareTool struct {
	client FileClient
	logger *slog.Logger
}

var _ tool.InvokableTool = (*FileShareTool)(nil)

func NewFileShareTool(c FileClient, logger *slog.Logger) (*FileShareTool, error) {
	if c == nil {
		return nil, fmt.Errorf("file client must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FileShareTool{client: c, logger: logger}, nil
}

func (t *FileShareTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "file_share",
		Desc: "为文件生成一个可分享的下载链接。" +
			"当用户要求分享文件、获取下载链接时使用。" +
			"需要先通过 file_search 获得 file_id。链接有有效期，过期后失效。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"file_id": {
				Type:     schema.Integer,
				Desc:     "文件 ID，可从 file_search 的结果中获得",
				Required: true,
			},
			"expire_hours": {
				Type: schema.Integer,
				Desc: fmt.Sprintf("链接有效期（小时），默认 %d，最大 %d（7 天，AWS 预签名上限）",
					defaultExpireHours, maxExpireHours),
			},
		}),
	}, nil
}

func (t *FileShareTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		FileID      uint `json:"file_id"`
		ExpireHours int  `json:"expire_hours"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.logger.Warn("could not parse file_share arguments", "raw", truncate(argsJSON, 200), "err", err)
		return fmt.Sprintf("参数解析失败：%s。file_id 为正整数，expire_hours 为整数。",
			truncate(err.Error(), 150)), nil
	}
	if args.FileID == 0 {
		return "参数 file_id 不能为空或 0。请先用 file_search 查到文件 ID。", nil
	}

	hours := args.ExpireHours
	if hours <= 0 {
		hours = defaultExpireHours
	}
	if hours > maxExpireHours {
		// 超上限时收敛并**告知模型**，让它能向用户说明为什么不是请求的时长
		t.logger.Info("clamping the share expiry", "requested", args.ExpireHours, "clamped", maxExpireHours)
		hours = maxExpireHours
	}

	url, err := t.client.Share(ctx, args.FileID, hours)
	if err != nil {
		t.logger.Warn("share creation failed", "file_id", args.FileID, "err", err)
		return fmt.Sprintf("创建分享链接失败（%s）。请如实告知用户，不要编造链接。",
			truncate(err.Error(), 200)), nil
	}

	msg := fmt.Sprintf("分享链接已创建，有效期 %d 小时：\n%s", hours, url)
	if args.ExpireHours > maxExpireHours {
		msg += fmt.Sprintf("\n（请求的 %d 小时超过了 %d 小时上限，已按上限处理）",
			args.ExpireHours, maxExpireHours)
	}
	return msg, nil
}

// ---- 格式化辅助 ----

// humanSize 把字节数转成人类可读的大小。
// 模型读「2.4 MB」比读「2516582」准确得多，回答里也更自然。
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// humanTime 把 Unix 秒转成可读时间。
// CloudKaho 的 created_at 是 epoch 整数而非 RFC3339。
func humanTime(epoch int64) string {
	if epoch <= 0 {
		return "未知时间"
	}
	return time.Unix(epoch, 0).Format("2006-01-02 15:04")
}
