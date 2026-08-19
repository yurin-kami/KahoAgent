//go:build jiebaspike

// 验证 gojieba（cgo）能否在本机构建并正确分词。
// 若不能，BM25 需要改用纯 Go 方案 —— 这个结论必须在写 BM25 之前拿到。
package main

import (
	"fmt"

	"github.com/yanyiwu/gojieba"
)

func main() {
	x := gojieba.NewJieba()
	defer x.Free()

	cases := []string{
		"Redis缓存穿透怎么解决",
		"布隆过滤器的应用场景",
		"使用 gojieba 对中文进行分词",
	}
	for _, s := range cases {
		fmt.Printf("%-30s -> %v\n", s, x.CutForSearch(s, true))
	}
}
