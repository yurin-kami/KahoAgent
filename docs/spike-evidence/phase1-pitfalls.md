# Phase 1 踩坑记录

集成测试发现的问题。两个都是**只有连真实服务才会暴露**的坑 —— mock 会按我
以为的语义返回，从而把 bug 一路放行到线上。这是坚持写集成测试的直接回报。

---

## 坑 1：Milvus VarChar 的 max_length 单位是字节，不是字符

### 现象

```
upsert 1 chunks: length of varchar field content exceeds max length,
row number: 0, length: 24576, max length: 8192: invalid parameter
```

### 根因

建表时写的是 `WithMaxLength(8192)`，我按「8192 个字符」理解；
截断函数也按 rune 截：

```go
// 错误实现
func truncateRunes(s string, maxLen int) string {
    r := []rune(s)
    if len(r) <= maxLen { return s }
    return string(r[:maxLen])
}
```

8192 个汉字 = 8192 × 3 = **24576 字节**，正好对应报错里的数字。
截断函数「成功」把内容切到 8192 字符后，服务端仍然拒收。

对纯 ASCII 文档这个 bug 永远不会触发 —— 这正是它危险的地方：
在中文知识库场景下才炸，而这恰好是本项目的主场景。

### 修复

1. 常量改名带上单位，让误读无法发生：
   `maxContentLen` → `maxContentBytes`，`maxIDLen` → `maxIDBytes`
2. 截断改为按字节，并回退到字符边界避免切出非法 UTF-8：

```go
func truncateBytes(s string, maxBytes int) string {
    if len(s) <= maxBytes { return s }
    cut := maxBytes
    for cut > 0 && !utf8.RuneStart(s[cut]) { cut-- }
    return s[:cut]
}
```

3. 上限从 8192 提到 32768 字节 —— 默认 chunk 为 500 字符（约 1500 字节），
   32KB 留出足够余量，实际几乎不会触发截断。

### 顺带修正的设计问题

写截断逻辑时意识到：**ID 绝不能截断**。
内容超长截断是可接受的降级（丢的是尾部文本），但 ID 截断会让两个不同 chunk
撞成同一主键，后写的静默覆盖先写的 —— 表现为「数据莫名其妙少了」，
比直接报错难查一个数量级。故 ID/DocID 改为超长即报错。

### 教训

> 凡是有长度上限的外部系统，先确认单位是字节还是字符。
> 常量名里写上单位（`maxContentBytes` 而非 `maxContentLen`），
> 让下一个人无法犯同样的错。

---

## 坑 2：Milvus SDK 在零命中时返回伪造的错误

### 现象

对空 collection 检索：

```
search result: extra output fields [id doc_id chunk_index content metadata]
found and result does not dynamic field
```

错误信息指向「output fields 配置错误」，与真实原因（没有命中）毫无关系，
极具误导性。

### 根因

读 SDK 源码 `client/data.go:198` 定位：

```go
if len(outputSet) > 0 && dynamicColumn == nil {
    return nil, errors.Newf("extra output fields %v found and result does not dynamic field", extraFields)
}
```

命中数为 0 时服务端不返回任何 `fieldsData`，SDK 的 `outputSet`
（待匹配的 output_fields 集合）自然一个都消不掉，于是走进这个分支
报出「多余字段」错误。这是 SDK 对空结果集的处理疏漏。

### 修复

判空必须在判错**之前**：

```go
// ⚠️ 顺序不能反
if r.ResultCount == 0 {
    return nil, nil
}
if r.Err != nil {
    return nil, fmt.Errorf("search result: %w", r.Err)
}
```

### 为什么这个坑必须现在解决

空结果不是异常，是**正常路径**：新部署的空库、Phase 2 加标量过滤后
过滤掉全部候选、用户查了个库里没有的话题 —— 都会走到这里。
若留到 Phase 3，表现为「Agent 偶发报一个看不懂的错」，
而那时调用链已经深到很难定位。

### 教训

> 第三方 SDK 的错误信息不一定指向真实原因。
> 报错内容和你的操作对不上时，直接读 SDK 源码定位抛错点，
> 比根据错误文本猜测快得多。

---

## 验证结果

```
=== RUN   TestMilvus_UpsertAndSearch                 --- PASS (3.42s)
=== RUN   TestMilvus_UpsertIsIdempotent              --- PASS (3.39s)
=== RUN   TestMilvus_DeleteByDoc                     --- PASS (10.42s)
=== RUN   TestMilvus_EnsureCollectionIsIdempotent    --- PASS (1.60s)
=== RUN   TestMilvus_RejectsMismatchedCounts         --- PASS (0.72s)
=== RUN   TestMilvus_RejectsWrongVectorDim           --- PASS (0.75s)
=== RUN   TestMilvus_SearchEmptyCollection           --- PASS (0.95s)
=== RUN   TestMilvus_LongContentIsTruncatedNotRejected --- PASS (3.60s)
=== RUN   TestMilvus_RejectsOversizedID              --- PASS (0.74s)
PASS
ok  github.com/yurin-kami/KahoAgent/pkg/vectorstore  27.367s
```

对应 Phase 1 step 4 验收项：

- [x] 上传 Markdown → Milvus 能查到对应 chunk（`doc_id` 匹配）
      → `TestMilvus_UpsertAndSearch`
- [x] 幂等验证：同文件上传两次，chunk 总数不变
      → `TestMilvus_UpsertIsIdempotent`
- [x] 删除验证：`DeleteByDoc` 后该文档 chunk 全消失
      → `TestMilvus_DeleteByDoc`

---

## 面试可讲的点

两个坑都是**具体、可复现、有源码级根因**的排查经历，比「我用过 Milvus」
有说服力得多：

- 字节/字符单位问题 → 体现对多字节编码的理解，以及「中文场景下才暴露」
  这一点对做中文知识库尤其有说服力
- SDK 空结果误报 → 体现读第三方源码定位问题的能力，而非卡在错误文本上
