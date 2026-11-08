// Package handler 提供 HTTP 接口。
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/metrics"
	"github.com/yurin-kami/KahoAgent/internal/pipeline"
)

// maxDocIDLen 限制文档 ID 长度。
//
// docID 会进入 Milvus 主键（sha256(docID#index) 的输入）和 PostgreSQL
// 的 VARCHAR(64) 外键列。这里限制 200 是留足余量：正常文件名远小于它，
// 而超长 docID 多半意味着客户端把整段路径甚至内容传上来了。
const maxDocIDLen = 200

// publishTimeout 是单次投递到 Kafka 的时限。
//
// 上传接口的价值是「立刻返回、后台慢慢处理」，所以这里不能等太久：
// Kafka 正常时投递是毫秒级，超过 15 秒说明 broker 已经不可用，
// 与其让用户干等，不如快速失败让他重试。
const publishTimeout = 15 * time.Second

// TaskPublisher 是上传接口对消息队列的最小依赖。
//
// 只声明一个方法，而不是直接依赖 *pipeline.Producer：
//   - 单测可以注入一个记录调用的假实现，不需要真 Kafka
//   - 将来换成别的队列（NATS / Pulsar）只改装配处，handler 不动
//
// 这是接口隔离原则的实际收益 —— handler 根本不关心消息怎么发出去。
type TaskPublisher interface {
	Publish(ctx context.Context, task pipeline.IngestTask) error
}

// UploadHandler 接收文档并投递到异步入库队列。
//
// 为什么是异步：一篇 5000 字的文档要切块 + 调一次 Embedding API，
// 实测 1~3 秒；同步做的话上传接口会被 embedding 的 P99 拖住，
// 而且用户看到的是「上传转圈」而不是「已收下，处理中」。
// 异步之后，接口的耗时只取决于 Kafka 的写入延迟。
type UploadHandler struct {
	pub     TaskPublisher
	logger  *slog.Logger
	maxRaw  int64
	nowFunc func() time.Time // 测试可替换时钟，生产恒为 time.Now
}

// NewUploadHandler 创建上传处理器。
func NewUploadHandler(pub TaskPublisher, logger *slog.Logger) (*UploadHandler, error) {
	if pub == nil {
		return nil, fmt.Errorf("publisher must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &UploadHandler{
		pub:     pub,
		logger:  logger,
		maxRaw:  pipeline.MaxRawBytes,
		nowFunc: time.Now,
	}, nil
}

// uploadJSON 是 application/json 请求体。
//
// 提供 JSON 入口不是为了替代 multipart，而是为了两件事：
//  1. curl 调试与单元测试不用构造 multipart 边界
//  2. 让「内容本身就是文本」的调用方（比如另一个服务）不必编码成文件
type uploadJSON struct {
	DocID   string `json:"doc_id"`
	Content string `json:"content"`
	// Filename 可选：doc_id 缺省时用它推断扩展名，
	// 因为分块策略是按扩展名选的（见 pipeline.ExtChunker）。
	Filename string `json:"filename"`
}

// ServeHTTP 按 Content-Type 分发到两个入口。
func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 用前缀匹配而不是全等：multipart 必然带 boundary 参数，
	// 而 JSON 可能带 charset=utf-8。
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		h.handleMultipart(w, r)
	case strings.HasPrefix(ct, "application/json"):
		h.handleJSON(w, r)
	default:
		http.Error(w, "unsupported content type: use multipart/form-data or application/json",
			http.StatusUnsupportedMediaType)
	}
}

// handleJSON 处理 JSON 入口。
func (h *UploadHandler) handleJSON(w http.ResponseWriter, r *http.Request) {
	// ⚠️ 必须限流读取，否则一个不闭合的请求体能一直占着内存。
	// 乘 2 是给 JSON 转义与字段名留余量；真正的原文长度在解码后再校验。
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRaw*2)

	var req uploadJSON
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.reject(w, metrics.UploadInvalid, "invalid JSON body", http.StatusBadRequest)
		return
	}

	docID := strings.TrimSpace(req.DocID)
	if docID == "" {
		docID = strings.TrimSpace(req.Filename)
	}
	h.accept(w, r.Context(), docID, []byte(req.Content))
}

// handleMultipart 处理 multipart/form-data 入口。
func (h *UploadHandler) handleMultipart(w http.ResponseWriter, r *http.Request) {
	// 表单开销（boundary、字段名、头部）远小于文件本体，
	// 给 256KB 余量足够；超过部分交给 MaxBytesReader 拒绝。
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRaw+256<<10)

	// 1MB 以内的部分放内存，超过的落临时文件 —— 但我们只关心一个文件，
	// 所以下面仍然会完整读出来再做长度校验。
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		// MaxBytesReader 触发的错误信息是 "http: request body too large"
		if strings.Contains(err.Error(), "request body too large") {
			h.reject(w, metrics.UploadTooLarge, "file exceeds the size limit", http.StatusRequestEntityTooLarge)
			return
		}
		h.reject(w, metrics.UploadInvalid, "invalid multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll() // 清理落盘的临时文件
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		h.reject(w, metrics.UploadInvalid, `missing "file" field`, http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	// 多读 1 字节：用它区分「正好等于上限」和「超过上限」，
	// 不必把整个超大文件读进内存才知道超了。
	raw, err := io.ReadAll(io.LimitReader(file, h.maxRaw+1))
	if err != nil {
		h.reject(w, metrics.UploadInvalid, "read upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	docID := strings.TrimSpace(r.FormValue("doc_id"))
	if docID == "" {
		docID = header.Filename
	}
	h.accept(w, r.Context(), docID, raw)
}

// accept 校验内容并投递任务，成功返回 202。
//
// 返回 202 而不是 200：任务只是「已受理」，尚未入库。
// 客户端若需要知道最终结果，应该轮询检索接口 —— 而不是在这里同步等待。
func (h *UploadHandler) accept(w http.ResponseWriter, ctx context.Context, docID string, raw []byte) {
	id, err := normalizeDocID(docID, raw)
	if err != nil {
		h.reject(w, metrics.UploadInvalid, err.Error(), http.StatusBadRequest)
		return
	}
	if len(raw) == 0 {
		h.reject(w, metrics.UploadInvalid, "document content must not be empty", http.StatusBadRequest)
		return
	}
	if int64(len(raw)) > h.maxRaw {
		h.reject(w, metrics.UploadTooLarge,
			fmt.Sprintf("document is %d bytes, exceeds the %d limit", len(raw), h.maxRaw),
			http.StatusRequestEntityTooLarge)
		return
	}
	// 二进制内容会被分块器当成 UTF-8 文本处理，切出来的全是乱码，
	// 而且 embedding 照样收费。与其让它污染索引，不如在入口挡掉。
	if looksBinary(raw) {
		h.reject(w, metrics.UploadBinary,
			"binary content is not supported (Markdown / plain text only)",
			http.StatusUnsupportedMediaType)
		return
	}

	// 投递用独立超时：不能直接复用 r.Context()，否则客户端一断开，
	// 消息可能刚好发到一半就被取消，而用户看到的是「上传失败」。
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	task := pipeline.IngestTask{DocID: id, Raw: raw}
	start := h.nowFunc()
	if err := h.pub.Publish(pubCtx, task); err != nil {
		// 记录完整错误，但只给客户端一个通用提示：
		// broker 地址、topic 名属于内部拓扑，不该出现在响应里。
		h.logger.Error("publish ingest task failed", "doc_id", id, "bytes", len(raw), "err", err)
		h.reject(w, metrics.UploadUnavailable, "ingest queue is unavailable, please retry later",
			http.StatusServiceUnavailable)
		return
	}

	metrics.RecordUpload(metrics.UploadAccepted)
	h.logger.Info("ingest task accepted",
		"doc_id", id, "bytes", len(raw), "publish_ms", h.nowFunc().Sub(start).Milliseconds())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"doc_id": id,
		"bytes":  len(raw),
		"status": "accepted",
	})
}

// reject 统一处理拒绝路径：记指标、写状态码。
func (h *UploadHandler) reject(w http.ResponseWriter, result, msg string, code int) {
	metrics.RecordUpload(result)
	http.Error(w, msg, code)
}

// normalizeDocID 校验并补全文档 ID。
//
// 为什么 docID 默认用文件名而不是内容哈希：
// 文件名是**文档的稳定身份** —— 同一篇文档改了几句话重新上传，
// 用文件名能覆盖旧版本（chunk 按 ID upsert，多出来的尾部 chunk
// 需要先删后写，见 Ingester 的说明）；用内容哈希则会生成新 docID，
// 旧版本永远留在索引里，检索结果里会出现同一篇文档的两个版本。
//
// 但这要求 docID 里不能有路径分隔符 —— 否则 DeleteByDoc 之类的
// 按 docID 操作会被构造出越权删除。
func normalizeDocID(docID string, raw []byte) (string, error) {
	docID = strings.TrimSpace(docID)

	// 缺省时用内容哈希兜底：至少保证幂等（同样内容 → 同样 docID），
	// 扩展名统一 .txt，交给 PlainChunker。
	if docID == "" {
		sum := sha256.Sum256(raw)
		return "upload-" + hex.EncodeToString(sum[:8]) + ".txt", nil
	}

	if len(docID) > maxDocIDLen {
		return "", fmt.Errorf("doc_id is too long (max %d bytes)", maxDocIDLen)
	}
	if strings.ContainsAny(docID, `/\`) || strings.ContainsRune(docID, 0) {
		return "", fmt.Errorf("doc_id must be a file name, not a path")
	}
	if docID == "." || docID == ".." {
		return "", fmt.Errorf("invalid doc_id")
	}
	return docID, nil
}

// looksBinary 通过 NUL 字节判断内容是否为二进制。
//
// 只检查前 512 字节：文本文件里出现 NUL 的概率极低，
// 而真正的二进制（PDF、图片、压缩包）几乎必然会带 NUL。
// 这不是严格的文件类型识别 —— 要严格识别就得引入 MIME 探测库，
// 对一个只接受 Markdown/文本的接口属于过度设计。
func looksBinary(raw []byte) bool {
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.IndexByte(head, 0) >= 0
}
