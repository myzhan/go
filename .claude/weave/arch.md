# weave 架构

> 本文给出 weave 的**整体结构视角**:系统由哪些部分组成、如何分层、一次测试如何在各层之间
> 流动、以及关键的组件与边界。设计取舍与理由见 [design.md](design.md);具体实现映射与状态见
> [imp.md](imp.md)。

## 定位

weave 是一个**单元级、封闭(hermetic)的并发交错验证器**:让一个 `go test` 单元测试系统性地
遍历多 goroutine 的交错执行,在无 `-race`、无 sleep、完全确定的前提下稳定复现"只在特定调度下
出现"的并发 bug(丢更新、死锁、破坏不变量、goroutine 泄漏、原子性违背),并给出可复现的最小
反例 + seed。

对标 Rust `loom` / 微软 Coyote / AWS Shuttle / CHESS,但**零改写**:未经修改、直接使用真实
`chan`/`sync.Mutex`/`select`/普通变量的代码就能被探索(靠运行时钩子 + 编译器插桩,而非强制换
类型)。同类工具的公认边界(依赖注入 / fake 掉真实 I/O 与时钟)同样适用,见 design.md 的 D9。

## 四层架构

```
┌──────────────────────────────────────────────────────────────┐
│ L4  testing/synctest    synctest.Test(t, f) + `go test -weave`  │  公共 API(=标准库)
│                        失败报告(trace + 图例 + seed)          │  无 weave 专属 API
├──────────────────────────────────────────────────────────────┤
│ L3  internal/weave     DPOR 探索引擎:跨 run 枚举交错          │  纯 Go,跑在 root/driver
│                        happens-before · conflict · seed 复现   │
├──────────────────────────────────────────────────────────────┤
│ L2  runtime 控制调度器  run-token 串行化 + 每个调度点选谁跑    │  runtime/weave.go
│                        拦截 ready/park_m/newproc/goexit        │  + proc.go/chan.go/select.go
│                        chan/select/sync 原语 = 调度点          │  + synctest.go + sync/*
├──────────────────────────────────────────────────────────────┤
│ L1  编译器插桩(-weave) 普通内存读写 → weaveread/weavewrite    │  cmd/compile(复用 -race pass)
│                        让数据竞态维度也可枚举                   │  + cmd/go 的 -weave flag
└──────────────────────────────────────────────────────────────┘
```

- **L2 是地基**:把未改写的 `chan`/`mutex`/`select`/`WaitGroup`/`Cond`/`Once` 变成受控调度点,
  并保证任一时刻只有一个参与者在跑。**始终编译进 runtime、无 build tag**,普通 `go test` 直接可用。
- **L1 是可选增强**:`-weave` 构建档把普通内存读写也变成调度点,让 DPOR 枚举数据竞态维度。
  仅在需要探索 plain 变量竞争(如无锁的丢更新)时才需要。
- **L3 是引擎**:纯 Go,不依赖 runtime 内部;经 `//go:linkname` 由 L2 提供的原语驱动。
- **L4 是入口**:**就是标准库 `testing/synctest`**——`-weave` 下 `synctest.Test` 改派到 L3 探索引擎
  (`weave_off.go`/`weave.go` 按 `weave` build tag 分流),失败结果渲染成可读报告。不再有独立的
  `testing/weave` 包(见 design.md D12)。

## 端到端数据流

一次 `synctest.Test(t, f)` 在 `-weave` 下的完整流动:

```
synctest.Test(t, f)  [-weave]                 [L4] weaveExplore → testingWeaveTest(f 作为 root 内联跑)
  └─ internal/weave.ExploreBounded(body)      [L3] DPOR 主循环,反复调用:
       └─ runSchedule(f, plan, trace...)       ── linkname ──▶ runtime.weaveRunSchedule  [L2]
            └─ 建 controlled synctest bubble,把 f 作为主参与者(parked)
               授予 run token,进入 run-token 调度:
                 参与者跑到调度点(chan/select/sync 原语、或 -weave 下的内存读写、Yield)
                   → weaveSchedPoint:记录 transition(wid/op/addr/enabled/pc/val)到 trace 缓冲
                   → 按 plan 重放或选最小 wid → 让出令牌给下一个参与者
                 真实阻塞(park_m)→ weaveOnBlock 交接令牌;无人可跑 = 死锁
                 参与者退出(goexit)→ weaveOnGoexit 交接;全部退出 = 完成
               全组阻塞/完成 → 唤醒 root,返回 (steps, 选择, outcome, panic)
       ◀── 带回 trace 缓冲 + outcome ──
       DPOR 回溯分析:对每对冲突且并发的 transition 加 backtrack → 生成下一条 plan
       命中 panic/死锁 → 停,构造 Result{Seed, Trace, Goroutines}
  └─ 失败:t.Errorf + 打印逐步交错 + goroutine 图例 + WEAVE_REPLAY=<seed> 复现命令
     成功:t.Logf "explored N schedule(s)";截断:报 INCOMPLETE
```

要点:**探索驱动(DPOR、panic 捕获)在 Go 侧(L3)**,runtime 只暴露"跑一条调度"的原语
`weaveRunSchedule`。这样引擎能用 `defer recover` 把模型 panic 捕获成"该调度失败",而不崩溃进程。

## 关键组件与边界

### synctestBubble.controlled + weaveControl

weave 不另造隔离机制,而是给现有 `synctestBubble` 加一个 `controlled bool` 与 `weaveCtl
*weaveControl`。`controlled=false` 时所有 weave 钩子惰性返回,bubble 行为与普通 synctest 完全一致。
`weaveControl` 持有单个 bubble 一条调度的全部状态:runnable 集合(并行数组,免写屏障)、
per-step trace 缓冲、`plan`/`selPlan`(重放向量)、存活计数、done/deadlock 标志、确定性 RNG 状态。

### run-token 串行化不变量

**任一时刻至多一个参与者处于 `_Grunning`。** 每个受控 bubble 恰有一个"运行令牌";参与者只有
持令牌才运行,到调度点交还令牌并 park,由控制器授予下一个。借用 runtime 既有的 `gopark`/
`goready` 作暂停/恢复,**不改 `schedule`/`findRunnable` 主逻辑**——等价于在真实 M:N 调度器上叠
一层协作式调度。令牌交接在三个中心点完成(无需逐原语改代码):`ready`(同步唤醒→入 runnable 集
而非 OS-runnable)、`park_m`(真实阻塞→交接令牌)、`goexit`/`Yield`(退出/显式让出→交接)。

### linkname 边界(runtime ↔ internal/weave)

```
runtime 暴露给 internal/weave(//go:linkname):
  weaveRunSchedule  → internal/weave.runSchedule   跑一条调度,带回 trace
  weaveYield        → internal/weave.Yield          显式调度点
  weaveWait         → internal/weave.Wait           等其余参与者退出
runtime 暴露给 sync / internal/sync:
  weaveSchedPoint       mutex/rwmutex/waitgroup/cond/once 入口记调度点
  weaveGloballyActive   单次全局 load 门控(见下)
```

### 门控:非 weave 程序零成本

两级门控保证普通程序几乎不付代价:

- `weaveActive()` = `gp.bubble != nil && gp.bubble.controlled`——热路径(chan/select)只多一次
  可预测的 not-taken 分支,与 synctest 同 footprint。
- `weaveGloballyActive`(受控 bubble 存在时才非 0)——供 `sync` 层做**单次全局 load** 门控,
  避免在每次 `Mutex.Lock` 里取 `getg().bubble`。

L2 因此**不需要 build tag**(而 -race 需要):L2 钩子只在 park/ready/newproc/goexit 等少数
choke point,可廉价运行时门控。只有 L1 内存插桩遍布每次访问,才需要 `-weave` 构建档。

## 关键数据结构

- **`weaveControl`**(runtime):一条调度的调度器状态。runnable 集合用并行定长数组
  (`runnable[guintptr]` + `runnableWid/Op/Addr/PC/Val`),使 `weaveEnqueue`(从 `ready` 调用,
  可能在禁写屏障处)无需写屏障、无分配。trace 缓冲(`traceWid/Op/Addr/Enabled/PC/Val` + select
  维度 `selTrace/selBranch/selStepIdx`)由 driver 传入切片,经 `weaveRunSchedule` 带回。
- **`Result`/`Step`/`Goroutine`**(internal/weave):`Result{Runs, Deadlock, Failed, Value,
  Seed, Trace, Goroutines, Truncated, TruncatedReason}`。`Step` 是一条 transition(wid/op/addr/
  file:line/值);`Goroutine` 把每个 `gN` 映射到其 `go` 语句创建位置(失败报告的图例)。

## 构建形态

| 命令 | 覆盖 | 说明 |
|---|---|---|
| `go test`(无 flag) | 一次普通 `synctest.Test` 单跑(不探索) | 标准库行为不变 |
| `go test -weave` | 同一 `synctest.Test` 用例 → 系统交错探索(含普通内存维度) | L1 插桩 + `synctest.Test` 改派 L3 |
| `go test -weave` | 上述 + **普通内存读写**(plain 变量、结构体字段、切片元素…) | L1 内存插桩,探索数据竞态;与 `-race`/`-msan`/`-asan` 互斥 |

`-weave` 等价于对命令行包加 `-gcflags=-weave` 并定义 `weave` 构建标签(仿 `-race` 定义 `race`)。
唯一尚未纳入的调度点是 `sync/atomic`(见 imp.md 的缺口说明)。
