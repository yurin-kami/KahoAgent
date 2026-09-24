//go:build corpus

// ⚠️ 本文件带 corpus tag，与被测的 ingest_corpus.go 一致。
// 普通 `go test ./...` **不会编译它**，验证必须用：
//
//	go test -tags=corpus ./scripts/eval/
//
// 不带 tag 跑会显示「no test files」而看起来像通过了 —— 这是个陷阱。
package main

import (
	"strings"
	"testing"
)

// docID 里必须保留扩展名，否则多格式支持会静默丢数据。
//
// ⚠️ 这是一条反证测试：验证它是否有效的唯一方法是把缺陷放回去
// （在 docIDFor 里恢复 strings.TrimSuffix(rel, filepath.Ext(rel))），
// 本测试必须变红。一个测不出 bug 的回归测试比没有更糟 ——
// 它给的是虚假的安全感。
func TestDocIDFor_DistinguishesExtensions(t *testing.T) {
	md := docIDFor("/corpus", "/corpus/redis/guide.md")
	pdf := docIDFor("/corpus", "/corpus/redis/guide.pdf")

	if md == pdf {
		t.Fatalf("docID 在 .md 与 .pdf 之间碰撞，两者都是 %q\n"+
			"后果：ChunkID = sha256(docID#index) 逐位相同，"+
			"而 Milvus 与 PG 都是 upsert —— 后入库的会静默覆盖先入库的", md)
	}
}

// 超长路径走哈希回退分支时，扩展名同样必须参与哈希。
//
// 这一条单独存在的理由：上一个测试只覆盖短路径（直接返回 name）。
// 哈希分支的基准是 rel 而非 name，若 rel 被剥掉扩展名，
// 超长路径下的碰撞会绕过上一个测试 —— 而且更隐蔽，
// 因为只在路径超过 60 字节时才触发。
func TestDocIDFor_LongPathHashIncludesExtension(t *testing.T) {
	// 造一个必然超过 maxBytes(60) 的路径
	long := strings.Repeat("nested/", 12) + "doc"

	md := docIDFor("/corpus", "/corpus/"+long+".md")
	pdf := docIDFor("/corpus", "/corpus/"+long+".pdf")

	if len(md) > 60 {
		t.Fatalf("docID 超出 60 字节预算：%d (%q)", len(md), md)
	}
	if md == pdf {
		t.Fatalf("哈希回退分支忽略了扩展名，两者都是 %q", md)
	}
}

// 原有的防碰撞保证不能因为这次修改而退化。
//
// en/ja 子目录同名文件曾实测丢过 62 个 chunk（见 docIDFor 注释），
// 这条守住那个修复。
func TestDocIDFor_DistinguishesDirectories(t *testing.T) {
	en := docIDFor("/corpus", "/corpus/en/getting-started.md")
	ja := docIDFor("/corpus", "/corpus/ja/getting-started.md")

	if en == ja {
		t.Fatalf("同名文件在不同目录下碰撞，两者都是 %q", en)
	}
}

// docID 必须是确定性的：同一输入永远产生同一输出。
// 这是整条管道幂等性的前提（重复入库覆盖而非堆积）。
func TestDocIDFor_IsDeterministic(t *testing.T) {
	const path = "/corpus/redis/guide.md"
	first := docIDFor("/corpus", path)
	for i := 0; i < 3; i++ {
		if got := docIDFor("/corpus", path); got != first {
			t.Fatalf("docID 不确定：第 %d 次得到 %q，首次是 %q", i+2, got, first)
		}
	}
}
