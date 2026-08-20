package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yurin-kami/KahoAgent/pkg/cloudkaho"
)

// fakeFileClient 按预设回应，并记录收到的参数。
type fakeFileClient struct {
	files    []cloudkaho.FileDetail
	detail   cloudkaho.FileDetail
	shareURL string
	err      error

	gotKeyword string
	gotLimit   int
	gotFileID  uint
	gotHours   int
}

func (f *fakeFileClient) Search(_ context.Context, keyword string, limit int) ([]cloudkaho.FileDetail, error) {
	f.gotKeyword, f.gotLimit = keyword, limit
	return f.files, f.err
}

func (f *fakeFileClient) Metadata(_ context.Context, fileID uint) (cloudkaho.FileDetail, error) {
	f.gotFileID = fileID
	return f.detail, f.err
}

func (f *fakeFileClient) Share(_ context.Context, fileID uint, hours int) (string, error) {
	f.gotFileID, f.gotHours = fileID, hours
	return f.shareURL, f.err
}

// ---- file_search ----

func TestFileSearch_ListsFilesWithIDs(t *testing.T) {
	// file_id 必须出现在结果里 —— 后续的 metadata / share 都要用它。
	// 不给的话模型只能瞎猜 ID 或再搜一次。
	f := &fakeFileClient{files: []cloudkaho.FileDetail{
		{FileID: 100, FileName: "redis.md", FileSize: 2048, CreatedAt: 1755600000},
	}}
	tl, err := NewFileSearchTool(f, quietLogger())
	if err != nil {
		t.Fatalf("new tool: %v", err)
	}

	out, err := tl.InvokableRun(context.Background(), `{"keyword":"redis"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, want := range []string{"redis.md", "file_id=100", "2.0 KB"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the output, got: %s", want, out)
		}
	}
	if f.gotKeyword != "redis" {
		t.Errorf("expected the keyword to be forwarded, got %q", f.gotKeyword)
	}
}

func TestFileSearch_LimitDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{`{}`, defaultFileLimit},
		{`{"limit":0}`, defaultFileLimit},
		{`{"limit":-1}`, defaultFileLimit},
		{`{"limit":9999}`, maxFileLimit},
		{`{"limit":3}`, 3},
	}
	for _, tc := range cases {
		f := &fakeFileClient{}
		tl, _ := NewFileSearchTool(f, quietLogger())
		if _, err := tl.InvokableRun(context.Background(), tc.args); err != nil {
			t.Fatalf("%s: %v", tc.args, err)
		}
		if f.gotLimit != tc.want {
			t.Errorf("%s: expected limit %d, got %d", tc.args, tc.want, f.gotLimit)
		}
	}
}

func TestFileSearch_NoResultsIsExplicit(t *testing.T) {
	f := &fakeFileClient{files: nil}
	tl, _ := NewFileSearchTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"keyword":"nope"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "没有找到") {
		t.Errorf("expected an explicit not-found message, got: %s", out)
	}
	// 关键词要回显，方便模型向用户复述搜的是什么
	if !strings.Contains(out, "nope") {
		t.Errorf("the keyword should be echoed back, got: %s", out)
	}
}

func TestFileSearch_FailureReportedAsObservation(t *testing.T) {
	// eino 对工具 error 会中止整个节点，所以失败必须变成文本
	f := &fakeFileClient{err: errors.New("connection refused")}
	tl, _ := NewFileSearchTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"keyword":"x"}`)
	if err != nil {
		t.Fatalf("must not return an error (eino aborts the run): %v", err)
	}
	if !strings.Contains(out, "失败") || !strings.Contains(out, "不要编造") {
		t.Errorf("the observation must report failure and forbid fabrication, got: %s", out)
	}
}

// ---- file_metadata ----

func TestFileMetadata_ReturnsSize(t *testing.T) {
	// 这是验收项「第二个文件多大」的能力基础
	f := &fakeFileClient{detail: cloudkaho.FileDetail{
		FileID: 100, FileName: "notes.md", FileSize: 24576, CreatedAt: 1755600000,
	}}
	tl, _ := NewFileMetadataTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"file_id":100}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// 既给人类可读的大小，也给精确字节数 ——
	// 前者让回答自然，后者保证用户追问时有准确值
	for _, want := range []string{"notes.md", "24.0 KB", "24576"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the output, got: %s", want, out)
		}
	}
	if f.gotFileID != 100 {
		t.Errorf("expected file_id 100 to be forwarded, got %d", f.gotFileID)
	}
}

func TestFileMetadata_RejectsZeroID(t *testing.T) {
	f := &fakeFileClient{}
	tl, _ := NewFileMetadataTool(f, quietLogger())

	for _, args := range []string{`{}`, `{"file_id":0}`} {
		out, err := tl.InvokableRun(context.Background(), args)
		if err != nil {
			t.Fatalf("%s: must not return an error: %v", args, err)
		}
		// 要引导模型先去搜，而不是干等着
		if !strings.Contains(out, "file_search") {
			t.Errorf("%s: should tell the model how to get an ID, got: %s", args, out)
		}
	}
}

// ---- file_share ----

func TestFileShare_ReturnsURL(t *testing.T) {
	f := &fakeFileClient{shareURL: "https://s3.example.com/abc"}
	tl, _ := NewFileShareTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"file_id":100,"expire_hours":2}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "https://s3.example.com/abc") {
		t.Errorf("expected the URL in the output, got: %s", out)
	}
	if f.gotHours != 2 {
		t.Errorf("expected 2 hours to be forwarded, got %d", f.gotHours)
	}
}

func TestFileShare_ExpiryDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{`{"file_id":1}`, defaultExpireHours},
		{`{"file_id":1,"expire_hours":0}`, defaultExpireHours},
		{`{"file_id":1,"expire_hours":-5}`, defaultExpireHours},
		{`{"file_id":1,"expire_hours":99999}`, maxExpireHours},
		{`{"file_id":1,"expire_hours":48}`, 48},
	}
	for _, tc := range cases {
		f := &fakeFileClient{shareURL: "u"}
		tl, _ := NewFileShareTool(f, quietLogger())
		if _, err := tl.InvokableRun(context.Background(), tc.args); err != nil {
			t.Fatalf("%s: %v", tc.args, err)
		}
		if f.gotHours != tc.want {
			t.Errorf("%s: expected %d hours, got %d", tc.args, tc.want, f.gotHours)
		}
	}
}

func TestFileShare_TellsModelWhenExpiryWasClamped(t *testing.T) {
	// 静默改成别的值会让 Agent 告诉用户「已按你要求设置 30 天」，
	// 而实际是 7 天 —— 那是在替模型撒谎
	f := &fakeFileClient{shareURL: "u"}
	tl, _ := NewFileShareTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"file_id":1,"expire_hours":720}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "上限") {
		t.Errorf("the model must be told the expiry was clamped, got: %s", out)
	}
}

func TestFileShare_FailureReportedAsObservation(t *testing.T) {
	f := &fakeFileClient{err: errors.New("s3 unavailable")}
	tl, _ := NewFileShareTool(f, quietLogger())

	out, err := tl.InvokableRun(context.Background(), `{"file_id":1}`)
	if err != nil {
		t.Fatalf("must not return an error (eino aborts the run): %v", err)
	}
	if !strings.Contains(out, "不要编造") {
		t.Errorf("the observation should forbid fabricating a link, got: %s", out)
	}
}

// ---- 共用 ----

func TestFileTools_RejectNilClient(t *testing.T) {
	if _, err := NewFileSearchTool(nil, nil); err == nil {
		t.Error("file_search: expected an error for a nil client")
	}
	if _, err := NewFileMetadataTool(nil, nil); err == nil {
		t.Error("file_metadata: expected an error for a nil client")
	}
	if _, err := NewFileShareTool(nil, nil); err == nil {
		t.Error("file_share: expected an error for a nil client")
	}
}

func TestFileTools_DescriptionsDistinguishFromRAG(t *testing.T) {
	// 模型最容易混淆的是「找文件」和「找文件里的内容」。
	// file_search 的描述必须划清这条界线，否则用户问
	// 「Redis 缓存怎么优化」时模型会去搜文件名。
	f := &fakeFileClient{}
	tl, _ := NewFileSearchTool(f, quietLogger())
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.Contains(info.Desc, "rag_query") {
		t.Errorf("file_search should point at rag_query for content search: %s", info.Desc)
	}
	if !strings.Contains(info.Desc, "不搜索文件内容") {
		t.Errorf("file_search should state it does not search content: %s", info.Desc)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{2048, "2.0 KB"},
		{24576, "24.0 KB"},
		{1048576, "1.0 MB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
	}
	for _, tc := range cases {
		if got := humanSize(tc.in); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanTime_HandlesMissingTimestamp(t *testing.T) {
	// CloudKaho 的 created_at 是 epoch 整数，0 表示没有值。
	// 直接格式化会得到 1970-01-01，那会误导用户。
	if got := humanTime(0); got != "未知时间" {
		t.Errorf("expected a placeholder for a zero timestamp, got %q", got)
	}
	if got := humanTime(-1); got != "未知时间" {
		t.Errorf("expected a placeholder for a negative timestamp, got %q", got)
	}
}
