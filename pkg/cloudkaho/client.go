// Package cloudkaho 是 CloudKaho 云盘服务的 HTTP 客户端。
//
// 只实现 Agent 工具需要的只读能力（搜索、详情、分享），
// 上传下载不在此列 —— 那些是大流量二进制操作，走 Agent 循环
// 会让 prompt 爆炸，应当由前端直连。
package cloudkaho

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultTimeout = 15 * time.Second

// Client 调用 CloudKaho 的 REST 接口。
//
// 认证用 JWT Bearer。Token 由调用方注入而非客户端自己登录 ——
// Agent 是代表**某个用户**在操作，token 必须跟着请求走，
// 客户端持有一个全局 token 会导致所有用户共用一个身份。
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type Option func(*Client)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

func New(baseURL, token string, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("baseURL must not be empty")
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// WithToken 返回换用另一个 token 的副本。
//
// 返回副本而非原地修改：多个会话可能并发使用同一个 Client，
// 原地改 token 会让 A 用户的请求带上 B 用户的身份。
func (c *Client) WithToken(token string) *Client {
	clone := *c
	clone.token = token
	return &clone
}

// FileDetail 与 CloudKaho 的 service.FileDetail 对应。
type FileDetail struct {
	RelationID     uint   `json:"relation_id"`
	FileID         uint   `json:"file_id"`
	FileName       string `json:"file_name"`
	ParentFolderID uint   `json:"parent_folder_id"`
	CreatedAt      int64  `json:"created_at"`
	FileSize       int64  `json:"file_size"`
	FileHash       string `json:"file_hash"`
	Status         int    `json:"status"`
}

// ⚠️ CloudKaho 的响应**没有统一信封**，且 code 是**字符串**不是数字。
// 成功为 "0"，失败为 "1"。各接口的数据键也不同
// （files / file / share_url），所以按接口分别定义响应类型，
// 而不是套一个通用 wrapper。
const codeOK = "0"

type errorBody struct {
	Code    string `json:"code"`
	Error   string `json:"error"`
	Details string `json:"details"`
}

type searchResponse struct {
	Code  string       `json:"code"`
	Files []FileDetail `json:"files"`
	Total int          `json:"total"`
}

type metadataResponse struct {
	Code string     `json:"code"`
	File FileDetail `json:"file"`
}

type shareResponse struct {
	Code     string `json:"code"`
	ShareURL string `json:"share_url"`
}

// Search 按文件名关键词搜索。keyword 为空时返回最近的文件。
func (c *Client) Search(ctx context.Context, keyword string, limit int) ([]FileDetail, error) {
	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	var out searchResponse
	if err := c.do(ctx, http.MethodGet, "/api/file/search?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// Metadata 取单个文件的详情（含大小）。
func (c *Client) Metadata(ctx context.Context, fileID uint) (FileDetail, error) {
	var out metadataResponse
	err := c.do(ctx, http.MethodGet,
		"/api/file/metadata/"+strconv.FormatUint(uint64(fileID), 10), nil, &out)
	return out.File, err
}

// shareRequest 对应 CloudKaho 的 ShareRequest。
type shareRequest struct {
	FileID    uint   `json:"file_id"`
	ShareType int    `json:"share_type"`
	ExpireAt  int64  `json:"expire_at,omitempty"`
	Password  string `json:"password,omitempty"`
	MaxViews  int    `json:"max_views,omitempty"`
}

const (
	// shareTypePublic = 2（1 是加密分享，需要密码）
	shareTypePublic = 2
	// maxShareHours 是 AWS 预签名 URL 的硬上限（7 天）。
	// 超过这个值 SDK 会直接报错。
	maxShareHours = 7 * 24
)

// Share 创建分享链接，返回预签名 URL。
//
// ⚠️ expireHours 会被换算成**绝对时间戳**再发出去。
// CloudKaho 的 expire_at 字段注释写的是「单位为秒，0 表示永不过期」，
// 但实现里是 time.Unix(expireAt, 0) 和
// time.Duration(expireAt - now) —— 即它当绝对时间戳用。
// 传一个小数字（或省略给 0）会算出**负数**有效期，
// 得到一个已过期或被拒绝的 URL。这个坑不看源码是发现不了的。
func (c *Client) Share(ctx context.Context, fileID uint, expireHours int) (string, error) {
	if expireHours <= 0 {
		expireHours = 24
	}
	if expireHours > maxShareHours {
		expireHours = maxShareHours
	}

	body := shareRequest{
		FileID:    fileID,
		ShareType: shareTypePublic,
		ExpireAt:  time.Now().Add(time.Duration(expireHours) * time.Hour).Unix(),
	}

	var out shareResponse
	if err := c.do(ctx, http.MethodPost, "/api/file/share", body, &out); err != nil {
		return "", err
	}
	if out.ShareURL == "" {
		return "", fmt.Errorf("share succeeded but returned no URL")
	}
	return out.ShareURL, nil
}

// codeCarrier 让 do 能统一检查业务码。
type codeCarrier interface{ okCode() string }

func (r searchResponse) okCode() string   { return r.Code }
func (r metadataResponse) okCode() string { return r.Code }
func (r shareResponse) okCode() string    { return r.Code }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		// ⚠️ 前缀检查在服务端是大小写敏感的 strings.HasPrefix(bearer, "Bearer ")，
		// 必须恰好一个空格
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return c.httpError(resp.StatusCode, raw)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w (raw: %s)", err, truncate(string(raw), 200))
	}

	// 业务码检查。HTTP 200 但 code="1" 是可能的 ——
	// 只看状态码会把失败当成功。
	if cc, ok := out.(codeCarrier); ok && cc.okCode() != codeOK {
		return fmt.Errorf("cloudkaho returned code %q", cc.okCode())
	}
	return nil
}

// httpError 把错误响应转成带上下文的 error。
func (c *Client) httpError(status int, raw []byte) error {
	var e errorBody
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		msg := e.Error
		if e.Details != "" {
			msg += ": " + e.Details
		}
		// 401 单独标注：这几乎总是 token 过期或没配，
		// 与「文件不存在」是完全不同的处理方式
		if status == http.StatusUnauthorized {
			return fmt.Errorf("认证失败（HTTP 401）：%s —— 请检查 CloudKaho token 是否配置且未过期", msg)
		}
		return fmt.Errorf("HTTP %d: %s", status, msg)
	}
	return fmt.Errorf("HTTP %d: %s", status, truncate(string(raw), 200))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
