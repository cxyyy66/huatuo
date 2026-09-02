# 大型符号表解析对比测试（PR #500 ELF 符号解析上限）

## 背景

PR #500 为 ELF 符号解析引入资源上限：第一次提交加入硬编码的符号数/元数据预算，
当前 HEAD 改为按请求地址按需解析（`elfSymbolsForPCs`），只物化覆盖目标 PC 的符号名。

生产现场问题：可执行文件约 573 MiB，perf 解析用户态符号期间进程 RSS 峰值约
8.5 GiB，随后 Huatuo Pod 被 OOM Killed。根因是 PR 之前的 `elfSymbols` 使用
debug/elf 的 `f.Symbols()` / `f.DynamicSymbols()` 全量物化所有符号（结构体 +
名字字符串），内存随符号数量线性放大，且 resolver 缓存常驻不释放。

## 测试 fixture

- 生成器：[gen_large_symtab.go](gen_large_symtab.go)（`//go:build ignore`），
  执行 `go run internal/symbol/testdata/gen_large_symtab.go` 会重新生成两个 fixture。
- fixture：[large_symtab_200m.elf](large_symtab_200m.elf)，200,194,760 字节（约 200MB）：
  - 560,000 条 `.symtab` + 56,000 条 `.dynsym`
  - 符号名 100-500 字符（平均约 300 字符），带索引前缀保证唯一
  - 符号类型混合：symtab 40% FUNC / 40% OBJECT / 20% NOTYPE，
    dynsym 60% FUNC / 40% OBJECT
  - 函数符号总数 257,600

### 100 条符号的参考 fixture

[symtab_100.elf](symtab_100.elf) 使用与 200MB fixture 相同的 ELF64 布局、符号命名
和类型分布，但只包含 90 条 `.symtab` 和 10 条 `.dynsym`，总计 100 条非空符号。
ELF 符号表要求的空索引项不计入该数量。该文件用于查看格式和调整测试参数，不用于
验证大型符号表的内存占用。

生产现场的普通 Linux ELF 可执行文件可以用于测试，但不能仅重命名为
`large_symtab_200m.elf`。替换 fixture 时还需要：

- 将 `largeSymPCs()` 改为文件内真实函数的 ELF 虚拟地址，而不是 ASLR 后的运行时地址；
- 将 `largeSymWants()` 改为这些地址对应的真实函数名；
- 按实际符号数量和 section 大小调整 `largeSymtabCount`、`largeDynsymCount` 和
  `ELFSymbolLimits`；
- 确认文件包含可读取的 `.symtab` 或 `.dynsym` 及其字符串表，完全 strip 的文件通常
  无法覆盖该测试场景。

## 测试方法

- 每个 commit 状态各一份 `internal/symbol/large_symtab_test.go`（API 随状态不同）。
- 解析 3 个已知函数地址，断言名字正确。
- 模拟 resolver 缓存常驻：按 exe / lib-1 / lib-2 三个缓存条目解析并全部持有，
  逐条记录累计峰值 RSS 增量。
- 内存指标：totalAlloc、heapAlloc、Sys、峰值 RSS（`/proc/self/status` VmHWM 增量）。

## 三个 commit 的结果

| 状态 | commit | 结果 |
|---|---|---|
| log-1：PR 之前 | `304050bb` | 每个缓存 257,600 符号；累计 RSS 增量 485 → 915 → 1003 MiB；3 次解析 totalAlloc 3372 MiB（约 17 倍文件）；GC 后仍常驻 273 MiB |
| log-2：第一次提交 | `b9e7a790` | 预检超限跳过，3 个缓存全部 0 符号；590µs；totalAlloc 0 MiB，RSS 增量 1 MiB |
| log-3：当前 HEAD | `5a467694` | 每个缓存仅保留 6 个按需符号；3 个缓存 RSS 增量 49 MiB；单次解析 36 MiB/op、44 次分配/op、约 40ms |

日志位置：

- [log-3.log](../../../_output/log-3.log)（HEAD，主仓库）
- `log-1.log`：`/home/huatuo/diff/huatuo-open-pr500-before/_output/log-1.log`
- `log-2.log`：`/home/huatuo/diff/huatuo-open-pr500-first/_output/log-2.log`

## 内存分析

### RSS 与文件大小、存活对象的关系

- RSS 是进程实际占用的物理页（高水位只增不减），不等于 Go 存活堆大小。
- 200MB fixture 在 PR 前单次解析 RSS 峰值约 500 MiB：200MB 原始 section 数据 +
  61.6 万条符号的结构体和名字字符串 + `io.ReadAll` 的缓冲区翻倍抖动。
- 三个缓存常驻累计约 1 GiB：三次解析的存活数据 + 瞬时垃圾 + 未归还操作系统的堆
  span（Sys 1228 MiB）叠加；GC 后净存活只有 273 MiB。
- 生产 573 MiB / 约 10M 符号按同样机制放大到 8.5 GiB；Pod 内存限制按 RSS 计算，
  所以即使存活集远小于峰值也会被 OOM Killed。

### HEAD 的内存节省点

| 内存来源 | PR 前（全量物化） | HEAD（按需解析） |
|---|---|---|
| 符号表原始数据（symtab + dynsym，约 15MB） | 读入 | 读入（找匹配地址必须） |
| 字符串表原始数据（strtab + dynstr，约 186MB） | 整块读入 | 完全不读，只按匹配 offset 流式取名字 |
| 全部符号名字（616k × 约 340B ≈ 210MB） | 全部物化 | 只物化 6 个匹配名字 |
| `elf.Symbol` 结构体（616k × 约 48B） | 全部生成 | 不生成 |
| FUNC 包装结构体（257.6k × 约 64B） | 全部生成 | 只生成匹配的 6 个 |

HEAD 的开销（约 36-49 MiB）不随符号数量或名字长度增长，这是与 PR 前线性放大的
本质区别。

## 结论

PR 前全量物化在大型符号表下内存放大显著且缓存常驻不释放，是生产 8.5 GiB 峰值
和 OOM 的直接原因；HEAD 的按需解析将单次解析控制在几十 MiB，缓存常驻可忽略。

参考 PR 只提交生成器、100 条符号的 fixture 和测试说明；200MB fixture 与实验日志
不提交。
