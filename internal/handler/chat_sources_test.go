package handler

import (
	"reflect"
	"testing"
)

// TestParseSources 锁住「工具输出 → 前端来源列表」的解析契约。
//
// 这段解析与 internal/agent/tools/rag_query.go 的 formatDocs 是隐式耦合：
// 那边改格式、这边不改，前端就永远看不到引用。所以用测试固定格式。
func TestParseSources(t *testing.T) {
	in := `【片段 1】来源: Redis 缓存实战 > 缓存击穿（redis.md）
缓存击穿是指...

【片段 2】来源: Redis 缓存实战 > 缓存雪崩（redis.md）
缓存雪崩是指...

【片段 3】来源: Redis 缓存实战 > 缓存击穿（redis.md）
重复来源应该被去掉
`
	got := parseSources(in)
	want := []string{
		"Redis 缓存实战 > 缓存击穿（redis.md）",
		"Redis 缓存实战 > 缓存雪崩（redis.md）",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

// TestParseSources_NoMatch 验证格式不符时返回空而不是 panic。
//
// 「未找到相关文档」是工具的另一种正常输出，前端不应该显示来源面板。
func TestParseSources_NoMatch(t *testing.T) {
	if got := parseSources("未找到相关文档。"); len(got) != 0 {
		t.Fatalf("sources = %v, want empty", got)
	}
	if got := parseSources(""); len(got) != 0 {
		t.Fatalf("sources = %v, want empty", got)
	}
}

// TestParseSources_CapsAtMax 验证来源数量有上限。
func TestParseSources_CapsAtMax(t *testing.T) {
	in := ""
	for i := 1; i <= 20; i++ {
		in += "【片段 " + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "】来源: doc" +
			string(rune('A'+i)) + "\n内容\n"
	}
	if got := parseSources(in); len(got) != maxSources {
		t.Fatalf("sources = %d, want %d", len(got), maxSources)
	}
}
