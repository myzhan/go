# weave — Go 并发交错穷举测试框架

`weave` 让一个单元测试能够**系统性地遍历多 goroutine 的交错执行**,从而在没有 `-race`、
没有 sleep、完全确定的前提下,稳定复现"只在特定调度下才出现"的并发 bug
(丢更新、死锁、破坏不变量、goroutine 泄漏),并给出**可复现的最小反例 + seed**。

目标定位:比 Rust `loom` 更强、更贴合 Go——让**未经改写**的真实 `chan` / `sync.Mutex` /
`sync/atomic` 代码就能被探索(靠运行时钩子 + 编译器插桩,而非强制换类型)。

## 文档索引

| 文件 | 内容 |
|---|---|
| [design.md](design.md) | 架构设计:四层结构、复用 synctest、控制调度器、编译器插桩、DPOR 引擎 |
| [roadmap.md](roadmap.md) | 分阶段路线图 M0–M6,每阶段任务清单 + 验收标准 + 状态 |
| [decisions.md](decisions.md) | 关键决策记录(命名、探索策略、内存模型范围、原型取舍等) |

## 当前状态(快照)

- **M0 原型引擎:已完成** ✅ — 见 `weaveproto/`(独立模块)。协作式串行调度器 + 穷举 DFS
  交错枚举 + 库级插桩原语(`Value`/`Mutex`)+ `weave.Test` API + 死锁检测 + 失败重放(seed)。
- **M2 控制器 + 真实同步点:已完成** ✅ — 已在真实 runtime 落地并验证:
  - `synctestBubble.controlled` + `weaveCtl` 字段;单个 `runtime/weave.go`(**始终编译、无 build
    tag**,`go test` 直接跑;成本靠运行时 `bubble.controlled` 门控,footprint 同 synctest)。
  - **run-token 控制器**(中心化,无需改 chan.go/sema.go):
    - `newproc` 受控下子 goroutine 生成即 park;
    - `ready` 拦截:被同步操作唤醒的参与者入控制器队列(`weaveEnqueue`),不直接 OS-runnable;
    - `park_m` 拦截:参与者真实阻塞(chan/mutex/…)时交接令牌(`weaveOnBlock`),无人可跑=死锁;
    - `goexit`/`Yield` 交接令牌;控制器自管存活计数、自判 done/deadlock;
    - `changegstatus`/`incActive`/`decActive` 对受控 bubble no-op;新增 `waitReasonWeaveScheduled`。
  - `internal/weave`:`Run`/`Yield` linkname 桥(始终编译)。
  - **验证**(`go test internal/weave`,**无 tag**):真实 `go func()` 串行定序、`Yield` 确定性交错、
    **真实 `chan` rendezvous**、**真实 `sync.Mutex`(含竞争 block→wake)**,各 50 次迭代稳定;
    普通构建 `synctest`/`sync`/`runtime`(chan/select/sema/sched/synctest/goroutine)**零回归**。
- **M3 跨 run 交错枚举:已完成(穷举版)** ✅ — `runtime/weave.go`:
  - 控制器的"选下一个"改为可插拔策略(`weaveChoose` 按 plan 重放 + 记录 choices/branches);
    runnable 改为 `guintptr` 定长数组(任意下标选择,无写屏障、无分配);
  - `weaveExplore`(linkname `internal/weave.Explore`):跨 run **DFS odometer**(`weaveNextPlan`)
    枚举所有交错,遇死锁停;`weaveRun` 仍是单条 FIFO 调度。
  - **验证**(`go test internal/weave`):`TestExploreCount` 枚举 6 条调度无误报;
    `TestExploreFindsDeadlock` 在**真实 `sync.Mutex`** 上找到 AB/BA 死锁;synctest/sync 零回归。
- **M4 panic 捕获 + `testing/weave` 公共 API:已完成** ✅
  - runtime 只暴露原语 `weaveRunSchedule`(跑一条调度);探索驱动移到 `internal/weave`(纯 Go),
    用 wrapper 闭包 `defer recover` **把模型 panic 捕获成"该调度失败"**(不崩溃进程);
  - `internal/weave.Explore` 返回 `Result{Runs, Deadlock, Failed, Value}`;
  - `src/testing/weave`:`Test(t, func())` + `Yield`,失败/死锁 → `t.Errorf`,成功 → `t.Logf`;
  - **验证**:`TestExploreFindsLostUpdate`(真实 chan join + Yield 暴露 RMW)→ 捕获丢更新 panic;
    `testing/weave.TestCorrect` 探索 4 条调度通过;synctest/sync 零回归。

**现在可用的最终形态**(`go test`,无 tag,真实类型):
```go
func TestCounter(t *testing.T) {
    weave.Test(t, func() {              // testing/weave
        x := 0
        done := make(chan bool, 2)      // 真实 channel 用作 join
        inc := func() { t := x; weave.Yield(); x = t + 1; done <- true }
        go inc(); go inc()              // 真实 go func
        <-done; <-done
        if x != 2 { panic("lost update") }  // 失败即报告可复现交错(seed 待接)
    })
}
```
- **M5 编译器内存插桩:已完成(通用内存/结构体)** ✅ — 对标 race detector:
  - `cmd/compile`:新增 `-weave` flag(`base.Flag.Weave`);`gc/main.go` 对 `NoInstrument` 包强制
    关掉(杜绝 runtime 自插桩递归);`ssagen/ssa.go` 的 `instrument2` 加 weave 分支(复用 race 的
    `s.load`/`s.store`/`instrumentFields`/`instrumentMove` 插入点)→ 插入 `runtime.weaveread/
    weavewrite/weavereadrange/weavewriterange`;`ir.Syms` 加对应符号。
  - `runtime/weave.go`:上述四个 hook,参与者访问 = 调度点(非参与者/driver 为 no-op,无递归)。
  - **覆盖**:普通变量、**结构体字段**、切片/数组元素、指针解引用、整struct拷贝(range),继承 race
    的成熟覆盖;跳过不逃逸栈局部/只读全局/零大小(与 race 一致,正确)。
  - **验证**(`go test -tags weaveinstr -gcflags=-weave internal/weave`):`TestAutoLostUpdate`
    (普通 `x=x+1`,**无 Yield**)、`TestAutoStructField`(`p.a=p.a+1`)均找到丢更新;
    普通构建(无 `-weave`)零回归,`go build std` 通过。
- **M6 DPOR(内存冲突,已验证健全)** ✅ — `internal/weave/explore.go`:
  - 每个参与者有稳定 `wid`(g.weaveWid,按创建序);控制器记录每步 transition
    (wid、op、地址、enabled 位掩码)到 trace 缓冲(`weaveRunSchedule` 新签名);
  - 经典 DPOR:向量化 happens-before 用**程序序**(保守=健全),回溯分析对每对**冲突**
    (同地址、≥1 写、跨参与者)的 transition 加回溯点,只探索每个偏序等价类的一个代表;
  - **健全性实测**:`TestDPOREquivalence`(`-weave`)——DPOR 与穷举**结论一致**(都找到丢更新)
    且 **DPOR 3 条 vs 穷举 12 条**;`TestExploreIndependent`——独立操作只探索 1 条;
    `TestAutoLostUpdate`/`StructField` 用 DPOR 找到 bug。
- **M7 channel sync-op 记录:已完成** ✅ — `runtime/chan.go` 的 chansend/chanrecv/closechan 在加锁前
  插 `weaveSchedPoint(ChanSend/Recv/Close, c)`(guarded,非 weave 程序零成本)→ channel 操作成为
  被记录的 transition,**DPOR 对 channel 冲突也健全**(无需 `-weave`)。
  - 同时修了一个 **wake-before-park 竞态**:`weaveOnGoexit`/`OnBlock` 唤醒 root 前 root 可能尚未 park
    (`bad g->status in ready`)。改为 `rootParked` 标志(在 park 的 unlockf 里、root 已 `_Gwaiting`
    后才置位)才唤醒,对齐 synctest 的 `maybeWakeLocked` 模式。
  - **验证**:`TestChannelDPOREquivalence`——channel 冲突下 **DPOR 19 vs 穷举 35**、结论一致;
    runtime(chan/select/synctest/sema)+ sync 零回归。
- **M8 mutex 记录 + 失败 seed 重放:已完成** ✅
  - **mutex 记录**:`internal/sync.Mutex.Lock/Unlock` 入口经 linkname 调 `runtime.weaveSchedPoint`
    (op=Lock/Unlock,addr=mutex),用全局 `weaveGloballyActive`(仅受控 bubble 存在时非 0)做**单次
    全局 load 门控** → 非 weave 程序几乎零成本。DPOR 现在对 **memory + channel + mutex** 冲突都健全。
    `TestMutexDPORFindsDeadlock`:DPOR 直接找到 AB/BA 死锁(17 条,给出 seed)。
  - **失败 seed 重放**:`Result.Seed/Trace`;`Replay(seed,f)`;`testing/weave.Test` 失败时打印逐步
    交错 + `WEAVE_REPLAY=<seed> go test -run ...`;支持 `WEAVE_REPLAY` 环境变量。
    `TestReplayReproduces`:seed 确定性复现同一交错。
- **M9 select 确定化 + RNG 确定化:已完成** ✅
  - **select**(#8):`runtime/select.go` 在受控 bubble 内跳过 `cheaprandn` 洗牌,poll order 用确定顺序
    → select 多就绪 case 的选择**可复现**(完整的 DPOR-over-select-cases 枚举留作增强)。
  - **RNG**(#9):`runtime/rand.go` 的 `rand()` 在受控 bubble 内(排除 root、全局门控)返回确定
    splitmix64 序列(`weaveRand`)→ **map 迭代序 / maphash 种子**等可复现。`TestMapIterationDeterministic`
    验证跨 run 一致。runtime select 测试 + sync/synctest 零回归。
- **最终用户形态:`weaveproto/`** ✅ — 删除原型库,`weave_test.go` 改为最终写法(真实
  `go func()`/`sync.Mutex`/`chan`/plain `int` + `testing/weave`,无任何特殊原语)。
  运行:`cd weaveproto && ../bin/go test -gcflags=-weave -v`。
  - `TestMutexProtected`/`TestChannelRendezvous`:正确代码 → **通过**;
  - `TestLostUpdate`(plain int)/`TestDeadlock`(mutex):**预期失败 = weave 报告 bug**,打印逐步
    交错 + `WEAVE_REPLAY=<seed>` 复现命令(已实测复现)。

- **唯一剩余任务:atomic 插桩(#6)**。深入分析后确认没有廉价做法:
  - 在 `type.go` typed 方法里记录 → 会撑破内联预算,**使所有 Go 程序的 typed atomic 去内联**(全局
    性能回退);
  - 关 intrinsic(`intrinsics.go:2392` 加 `|| Weave`)+ 在 `sync/atomic` 记录 → 同样有去内联/需
    重建 std 的问题;
  - atomic intrinsic 是 ~30 个按操作×按架构的发射点,无中心 hook。
  - **正确做法(仿 -race)**:引入 `-weave` 构建模式,用带记录的 `sync/atomic` 重建 std(像 -race
    的 race-instrumented std),对非 weave 构建零影响。属较大工程,留作独立专注实现,未草率合入以免
    拖累全体 Go 程序的 atomic 性能。
- 长期增强:DPOR-over-select-cases;抢占看门狗(死循环兜底);cmd/go 的 `-weave` flag(自动传播,
  免手写 `-gcflags`,并驱动上面的 atomic std 重建)。

详细进度见 [roadmap.md](roadmap.md);L2 落地细节见 [l2-impl-notes.md](l2-impl-notes.md)。

## 快速跑通原型

```sh
cd weaveproto
go test -v            # 4 类用例:丢更新(找到)、加锁(无误报)、死锁(找到)、重放(复现一致)
```

失败时会打印逐步交错和一个 seed,例如:

```
reproduce with: WEAVE_REPLAY=0.0.0.0.1.1.0.0.0.0.0 go test -run TestLostUpdate -v
```

用该 seed 可确定性重放**逐字节相同**的交错。

## 用户如何写测试(目标形态)

```go
func TestCounter(t *testing.T) {
    weave.Test(t, func(t *testing.T) {   // 目标:真实类型,零改写
        var x int
        var mu sync.Mutex
        go func() { mu.Lock(); x++; mu.Unlock() }()
        go func() { mu.Lock(); x++; mu.Unlock() }()
        weave.Wait()
        if x != 2 { t.Errorf("x = %d, want 2", x) }
    })
}
```

约束(同 loom/synctest):模型闭包必须**可重跑**(共享状态在闭包内声明)、除调度外**确定性**
(勿用 `rand`/真实时间/依赖 map 迭代序)、所有 goroutine 必须能结束。
