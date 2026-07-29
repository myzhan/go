# weave — Go 并发交错穷举测试框架

`weave` 让一个单元测试能够**系统性地遍历多 goroutine 的交错执行**,从而在没有 `-race`、没有
sleep、完全确定的前提下,稳定复现"只在特定调度下才出现"的并发 bug(丢更新、死锁、破坏不变量、
goroutine 泄漏、原子性违背),并给出**可复现的最小反例 + seed**。

目标定位:比 Rust `loom` 更贴合 Go——让**未经改写**的真实 `chan` / `sync.Mutex` / `select` /
普通变量代码就能被探索(靠运行时钩子 + 编译器插桩,而非强制换类型)。它是**单元级、封闭
(hermetic)的并发验证器**;对标 loom/Coyote/Shuttle/CHESS,同类工具的公认边界(fake 掉真实
I/O 与时钟)同样适用。

## 文档索引

| 文件 | 内容 |
|---|---|
| [arch.md](arch.md) | **整体架构**:四层结构、端到端数据流、关键组件与边界、构建形态 |
| [design.md](design.md) | **设计文档**:设计原则、各层设计、健全性论证、与 -race 关系、ADR 决策记录、范围边界 |
| [imp.md](imp.md) | **实现与状态**:文件/符号映射、关键运行时机制、里程碑状态、已知坑、验证方法 |
| [proposal.md](proposal.md) | 初始提案 |

## 当前状态

已达到**可用里程碑**:未改写的真实 `chan`/`select`/`sync.Mutex`/`RWMutex`/`WaitGroup`/`Cond`/
`Once` + 普通内存(`-weave`)代码可被系统性交错探索;DPOR(含差分健全性验证 + 抢占上界)、
select-case 枚举、非阻塞 channel 操作、RNG/map 序确定化、panic 捕获、失败 seed 重放 + 源码行号 +
goroutine 图例 + 读写值显示均已落地。**易用性**:不设抢占上界时**默认自动迭代加深**(0→2,避免真实
模型爆炸)、失败轨迹对象用**可读标签**(mutex#1/chan#2)、**死锁报告逐 goroutine 列出等待对象**、
假时钟"超时 vs 事件"漏探时**打印对齐提示**。**API 已精简**:不再有 `testing/weave`/`weave.Test`——直接用
标准 `testing/synctest.Test`,`-weave` 下自动进入探索(ADR D12)。`sync/atomic` **类型化 API**
(`atomic.Int64/Bool/Pointer` 等的方法)已插桩为调度点(ADR D18,库级钩子 + build-tag no-op 保内联),
无锁 RMW 能探索了;**剩余缺口**:atomic 自由函数(`atomic.LoadInt64` 等 intrinsic)与 `atomic.Value`。

详细能力清单与里程碑见 [imp.md](imp.md) §1、§6。

## 快速跑通

```sh
# 核心引擎 + 健全性套件
cd src && ../bin/go test internal/weave/ testing/synctest/
cd src && ../bin/go test -weave internal/weave/

# 演示模块:全部是普通 testing/synctest 用例,加 -weave 即进入系统交错探索
# (部分用例为故意失败,展示 weave 抓 bug + 打印 seed)
cd weavedemo && ../bin/go test -weave -v
```

失败时打印逐步交错 + goroutine 图例 + 一个 seed,例如:

```
reproduce with: WEAVE_REPLAY=0.0.0.0.1.1.0.0.0.0.0 go test -weave -run '^TestLostUpdate$'
```

用该 seed 可确定性重放逐字节相同的交错。

## 用户如何写测试

**没有 weave 专属 API**——就写标准 `testing/synctest` 用例,加 `go test -weave` 即让同一个用例
跑进系统化交错探索(类比 `-race` 是模式而非 API,见 [design.md](design.md) D12)。

```go
func TestCounter(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {   // 标准库 testing/synctest,真实类型、零改写
        var x int
        var mu sync.Mutex
        go func() { mu.Lock(); x++; mu.Unlock() }()
        go func() { mu.Lock(); x++; mu.Unlock() }()
        synctest.Wait()                      // 等其余 goroutine 退出后再断言
        if x != 2 { t.Errorf("x = %d, want 2", x) }
    })
}
```

`go test`(不带 -weave)= 一次普通 synctest 单跑;`go test -weave` = 同一用例的系统交错探索。

约束(同 loom/synctest):模型闭包必须**可重跑**(共享状态在闭包内声明)、除调度外**确定性**
(勿用 `rand`/依赖 map 迭代序)、所有 goroutine 必须能结束。时间/定时器由 synctest 假时钟驱动,
`time.Sleep`/`time.After`/`context.WithTimeout` 在受控 bubble 内**可正常工作**(见 [design.md](design.md)
D11);仍建议用 channel/`context.WithCancel` 等内存接缝让交错更显式。

## 搜索深度(抢占上界)

不设 `WEAVE_MAX_PREEMPTIONS` 时,weave **自动迭代加深**:依次探索 0,1,…,2 次抢占的调度,
找到失败即停(反例用最少抢占,最易读),否则报 "explored N schedule(s) up to 2 preemption(s)"。
这避免了在真实模型上无界爆炸。**大多数并发 bug 在 ≤2 次抢占内出现**(CHESS 经验);若某 bug
需要更深(如跨多次让出的竞争),用 `WEAVE_MAX_PREEMPTIONS=<c>` 提高上界(会更慢)。
`WEAVE_MAX_SCHEDULES` / `WEAVE_TIMEOUT` 是总预算,耗尽则报 INCOMPLETE(注明探到第几层)。

## 测网络 / IO 逻辑

真实 socket 会离开 bubble——weave 无法控制或探索它(同 loom/Coyote/CHESS 的边界)。做法是把
**传输换成内存 fake**,让 weave 探索其上的交错:

- **首选 `net.Pipe`**:它本就是 bubble-aware(channel 实现),且**紧凑**——Read/Write 是同步
  rendezvous,每次交接一个调度点,探索空间小。请求/响应、协议握手、断线重连都能直接跑在它上面
  (见 `weavedemo/netfake_test.go`、`asyncio_test.go`)。
- **别用 `sync.Cond`+`[]byte` 自造 buffered conn**:`-weave` 下每次 slice/标志访问都成调度点,
  fake 自身的内部状态就会撑爆搜索(一个单字节回显都可能超预算)。确需缓冲就用 **buffered channel**
  (每条消息一个调度点),不要 cond+slice。
- **超时/重连**:用 `context`/channel 表达超时,而不是依赖真实时间;重连建模为"替换当前 conn +
  通知"(mutex/cond),weave 会探索"旧 conn 出错 vs 重连接管"的交错。

### 假时钟与"超时 vs 事件"竞争的一个坑

假时钟只在 bubble **全体 durably blocked** 时才推进(推进到最近的 pending timer)。因此若被测的
并发事件对应的 goroutine **一直可运行**,假时钟不会推进,基于 `time.After` 的超时**永远不会触发**,
"超时 vs 事件"这一类竞争就探不到(假阴性)。要探这类竞争,把并发事件**对齐到定时器边界**——例如
让事件 goroutine 先 `time.Sleep(超时时长)` 到与超时同一虚拟时刻,两者便在同一时刻竞争,weave 即可
交错二者。(weave 在探索结束若发现有从未触发的定时器,会打印提示引导你这样做。)
