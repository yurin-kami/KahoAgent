package tokenizer

import (
	"strings"
	"testing"
)

func TestTokenize_SplitsChineseCompounds(t *testing.T) {
	// 核心用途：PG 自带分词器会把「Redis缓存穿透」当成一个词，
	// 导致查「缓存穿透」匹配不上。分词后必须能切开。
	got := Shared().Tokenize("Redis缓存穿透怎么解决")

	for _, want := range []string{"Redis", "缓存", "穿透"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected token %q in %q", want, got)
		}
	}
}

func TestTokenize_SearchModeProducesSubwords(t *testing.T) {
	// CutForSearch 会在长词基础上额外切子串，
	// 这样查「过滤器」也能命中「布隆过滤器」
	got := Shared().Tokenize("布隆过滤器")

	if !strings.Contains(got, "过滤器") {
		t.Errorf("expected the full word 过滤器 in %q", got)
	}
	if !strings.Contains(got, "布隆") {
		t.Errorf("expected 布隆 in %q", got)
	}
}

func TestTokenize_HandlesMixedChineseEnglish(t *testing.T) {
	got := Shared().Tokenize("使用 gojieba 对中文进行分词")

	if !strings.Contains(got, "gojieba") {
		t.Errorf("English token lost: %q", got)
	}
	if !strings.Contains(got, "分词") {
		t.Errorf("Chinese token lost: %q", got)
	}
}

func TestTokenize_NoRedundantWhitespace(t *testing.T) {
	// gojieba 会把原文空格也当词返回，必须过滤掉
	got := Shared().Tokenize("使用 gojieba 对中文进行分词")

	if strings.Contains(got, "  ") {
		t.Errorf("consecutive spaces in output: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("output has leading/trailing space: %q", got)
	}
	for _, f := range strings.Split(got, " ") {
		if f == "" {
			t.Errorf("empty token in output: %q", got)
		}
	}
}

func TestTokenize_Empty(t *testing.T) {
	if got := Shared().Tokenize(""); got != "" {
		t.Errorf("expected empty output, got %q", got)
	}
	if got := Shared().Tokenize("   "); got != "" {
		t.Errorf("whitespace-only input should produce no tokens, got %q", got)
	}
}

func TestTokenizeQuery_UsesOrNotAnd(t *testing.T) {
	// 用 AND 连接会让召回率趋近于 0 —— 几乎没有文档能同时含全部词
	got := Shared().TokenizeQuery("Redis 缓存穿透怎么解决")

	if !strings.Contains(got, "|") {
		t.Errorf("expected OR-joined tsquery, got %q", got)
	}
	if strings.Contains(got, "&") {
		t.Errorf("tsquery must not use AND, got %q", got)
	}
}

func TestTokenizeQuery_QuotesEveryToken(t *testing.T) {
	got := Shared().TokenizeQuery("缓存 穿透")

	for _, part := range strings.Split(got, "|") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "'") || !strings.HasSuffix(part, "'") {
			t.Errorf("token %q is not quoted in %q", part, got)
		}
	}
}

func TestTokenizeQuery_EscapesTsqueryOperators(t *testing.T) {
	// tsquery 的语法字符不能破坏查询结构，否则会是注入风险
	for _, input := range []string{
		"缓存 & 穿透", "a | b", "test:*", "(foo)", "!bar", "it's",
	} {
		got := Shared().TokenizeQuery(input)
		if got == "" {
			continue
		}
		// 每个词元都被单引号包裹，运算符只可能出现在引号之间
		for _, part := range strings.Split(got, "|") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.HasPrefix(part, "'") || !strings.HasSuffix(part, "'") {
				t.Errorf("input %q produced an unquoted token %q (full: %q)", input, part, got)
			}
		}
	}
}

func TestTokenizeQuery_Empty(t *testing.T) {
	if got := Shared().TokenizeQuery(""); got != "" {
		t.Errorf("expected empty tsquery, got %q", got)
	}
}

func TestShared_ReturnsTheSameInstance(t *testing.T) {
	// 词典约 100MB，每次新建会迅速吃光内存
	if Shared() != Shared() {
		t.Error("Shared() must return a single cached instance")
	}
}

func TestTokenize_IsConcurrencySafe(t *testing.T) {
	// 检索路径会并发调用，gojieba 的分词方法必须能共用同一实例
	const n = 50
	done := make(chan string, n)

	for range n {
		go func() { done <- Shared().Tokenize("Redis缓存穿透怎么解决") }()
	}

	first := <-done
	for range n - 1 {
		if got := <-done; got != first {
			t.Fatalf("concurrent tokenization differs:\n  %q\n  %q", first, got)
		}
	}
}
