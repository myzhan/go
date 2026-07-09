# weave 架构设计

> 命名 `weave`(编织):goroutine 即线,探索交错 = 把多根线编织起来看所有织法。
> 与 `synctest` 成对:`synctest` 负责隔离+假时钟,`weave` 负责交错枚举。
> 本文中 "Rust loom" 专指作为参考的 Rust 原版工具。

## 问题与目标

系统性遍历多 goroutine 交错,确定性复现"只在特定调度下出现"的并发 bug。相对 Rust loom
的两点改进(已与用户确认):

1. **Go-native / 零改写**:loom 强制把 `std::sync` 换成 `loom::sync`(靠 `cfg(loom)` 条件编译),
   这是它最大的工程学负担。weave 要让**未修改的、用真实 `chan`/`sync.Mutex`/`sync/atomic`
   的代码**直接被探索——靠深度复用运行时 `synctest` + 编译器插桩(类比 `-race`)。
2. **探索引擎目标是 DPOR**(动态偏序规约),而非停留在随机/穷举。

## 复用基础:runtime 已有的 synctest

Go 已有 `synctest` bubble 机制,提供了 weave 所需一半的能力:

- `src/runtime/synctest.go` — `synctestBubble`:一组 goroutine 的隔离集合,跟踪
  `total/running/active`,并在 `changegstatus`(由 `casgstatus` 调用,`proc.go:1290/1329`)
  里检测"**整组是否都 durably blocked**"(全组静止)。这是 DPOR 判定"一次运行结束/到达
  静止点"的关键信号。
- `src/internal/synctest/synctest.go` — 通过 `//go:linkname` 桥接 `Run/Wait/Associate/...`。
- `src/testing/synctest/synctest.go` — 公共 API `synctest.Test(t, f)`。
- bubble 归属**在 goroutine 创建时无条件继承**:`proc.go:5401` `newg.bubble = callergp.bubble`。
  → 从测试闭包(直接/间接)派生的 goroutine,不论在哪个 package、是否用特殊类型,一诞生就
  进 bubble,被 weave 控制。**边界是"是否从 bubble 内派生",不是 package。** 进程级、
  bubble 外预先存在的 goroutine(init/单例/全局池)不在其内,需在闭包内构造或用 fake。
- 各同步原语**已有 bubble 专用代码路径**,是天然插桩点:
  - `chan.go`:send(~280)、recv(~664)、close;`select.go`(~200);`sema.go`(mutex/waitgroup)。

synctest **缺**、weave 要补的另一半:
1. **串行化**:bubble 内 goroutine 仍在多 P 上真并行;weave 要求任一时刻只有一个在跑。
2. **调度顺序控制**:每个调度点由外部控制器决定下一个跑谁。
3. **跨多次运行的系统枚举**:DPOR 跨 run 生成不同调度。

关键洞察:loom / CHESS 都**只在同步操作处切换**,不在任意指令处切换(任意指令处的非原子
内存竞态交给 race detector)。Go 里"同步操作处"恰好对应 `gopark`/`goready` 边界 +
chan/sema/select 的 bubble 路径 + goroutine 起止。控制器靠拦截这些既有 choke point 即可,
**无需重写 M:N 调度器**。

## 四层架构

```
┌─────────────────────────────────────────────────────────┐
│ L4  testing/weave     weave.Test(t, func(t *testing.T))  │  公共 API
├─────────────────────────────────────────────────────────┤
│ L3  internal/weave    DPOR 引擎 + happens-before 向量钟   │  纯 Go,跑在 root goroutine
├─────────────────────────────────────────────────────────┤
│ L2  runtime 控制调度   run-token 串行化 + 调度决策回调      │  runtime/weave.go + synctest.go
│                       (拦截 goready/park + chan/sema/sel) │  + proc.go/chan.go/sema.go/select.go
├─────────────────────────────────────────────────────────┤
│ L1  编译器插桩         atomic / 共享内存读写 → 事件回调     │  cmd/compile 新 pass(类比 -race)
└─────────────────────────────────────────────────────────┘
```

L2 让未改写的 chan/mutex/waitgroup/cond 可被交错探索;L1 把 atomic/内存也纳入;L3 是引擎;
L4 是入口。**原型阶段(M0)用库级插桩原语在纯 Go 里实现了 L3+L4**,验证引擎与 API;后续把
原语底层换成 L2/L1 的钩子,兑现零改写。

## L2:控制调度器

在 `synctestBubble` 上加 `controlled bool` 与控制器句柄。开启后:

1. **run-token 串行化**:每个 controlled bubble 恰有一个运行令牌,bubbled goroutine 只有持
   令牌才 `_Grunning`。到达调度点释放令牌、上报事件、park,等控制器再次授予。等价于在真实
   M:N 调度器上叠一层协作式调度,借用 `gopark`/`goready` 作暂停/恢复——**不改
   findRunnable/schedule 主逻辑**。
2. **拦截 ready**:controlled 下 `goready`/`ready`(`proc.go:493/1133`)不入 runqueue,而是加入
   控制器 enabled 集合;控制器挑唯一一个 resume。
3. **调度决策回调**:`weave_choose(enabled []goid) goid`(经 linkname 注册到 L3),DPOR 在此插策略。
4. **同步事件上报**:在 chan/sema/select 现有 bubble 分支,阻塞前/成功后调
   `weave_event(op, objID, ...)`;objID 复用 `getOrSetBubbleSpecial` 编号。
5. **调度点**:goroutine 创建(`newproc1`,`proc.go:5401`)、退出、每次同步操作前后、
   `weave.Wait`;全组 durably-blocked(复用 `maybeWakeLocked`,`synctest.go:132`)= 一次 run 的
   静止/结束点。

新增 `src/runtime/weave.go` 放控制器状态机 + linkname 钩子;`synctest.go` 加字段;
`proc.go`/`chan.go`/`sema.go`/`select.go` 在 bubble 分支加 `if bubble.controlled` 最小分叉。
新增锁需在 `mklockrank.go` 登记。

## L1:编译器插桩(atomic / 共享内存)

`sync/atomic` 是编译器 intrinsic,无调用点,靠 linkname 拦不住。参照 race detector
(`-race` → 编译器插入 `raceread`/`racewrite`,pass 在 `cmd/compile/internal/ssagen` + `walk`):

- 新增构建模式 `-weave`(`go test -weave` / `-gcflags=-d=weave`),递归插桩所有依赖(同 `-race`):
  - 每个 atomic op → `runtime.weaveatomic(op, addr, ...)`:成为调度点 + 事件。
  - (进阶,强于 loom)每个共享内存读/写 → `runtime.weaveread/weavewrite(addr)`:让 DPOR 在数据
    竞态处也能切换。默认只插 atomic,内存读写作 `-weave=race` 高强度档。
- 弱内存建模(relaxed load 返回旧值,枚举 C11 重排):`weaveatomic` 的 load 由 L3 从该地址允许
  的历史写集合中选值返回。作为最高阶段;MVP 先做顺序一致的 atomic 调度点。

参考:`src/runtime/race.go`;内存模型 happens-before 权威定义见 `doc/go_mem.html`。

### 已落地:与 sanitizer 共享同一条插桩 pass(instrumentation fusion)

`-weave` 的内存读写插桩**没有另起炉灶**,而是复用 race/msan/asan 用的同一条 pass
(`cmd/compile/internal/ssagen/ssa.go` 的 `instrument2`),只在末尾按模式发不同的 hook:

- `base.Flag.Weave` → `Cfg.Instrumenting = true`(`gc/main.go`),走 `s.instrumentMemory` 同一门控;
  composite/scalar 分流、`weaveread/weavewrite` vs `weavereadrange/weavewriterange` 与 race 对称。
- **白拿逃逸剪枝**:`instrument2` 开头统一调 `ssa.IsSanitizerSafeAddr(addr)`,栈地址/RODATA/闭包
  只读字段等一律不插桩。不逃逸的 goroutine-local 变量落在栈上 → 被剪掉,不会变成调度点——对
  weave 这是 sound 的(局部量不可能被别的 goroutine 观察,重排它天然 independent),直接缩状态空间。
- **runtime 不被插桩**:`objabi.NoInstrument` 包(runtime 等)在 `gc/main.go` 里把 Weave 关掉,
  避免 `weaveread` 递归回自身。
- **与 sanitizer 互斥**(本次补的唯一缺口):`instrument2` 的 if/else 先判 race/msan/asan 再判
  weave,同开会静默丢掉 weave 插桩。已加双重保护——`cmd/go` 的 `weaveInit`(友好报错)+
  `cmd/compile` flag 校验(`cannot use both -weave and -race/...`),两层都验证过。

即"一套插桩,两个后端":共享插入点 + 剪枝,hook 语义与构建模式各自独立。sync 对象(chan/mutex/
waitgroup)不走编译器,由 runtime 的 park/ready 钩子处理,不在这条 pass 内。

## L3:探索引擎(DPOR)

新增 `src/internal/weave/`,纯 Go,跑在 root goroutine,经 linkname 被 L2 回调:

- **状态**:每 run 记录 transition 序列(g、op、objID)。为每个同步对象/内存地址维护向量钟,
  按 `go_mem.html` 规则算 happens-before,判定两 transition 是否冲突(同对象、至少一写、
  且不 happens-before)。
- **DPOR 主循环**(source-DPOR / optimal-DPOR):跑一条 schedule 到静止/结束 → 回溯扫描,对每
  对冲突且并发的 transition 在其竞争前驱的调度点 backtrack 集合里加入被延迟的 g → 从
  backtrack 驱动下一条 schedule,直到耗尽。只探索每个偏序等价类的一个代表,指数级剪枝。
- **回调面**:`weave_choose(enabled) goid`、`weave_event(...)`、`weave_runStart/runEnd`。
- **可选上界**:抢占计数上界(iterative context bounding / CHESS 式)兜底超大状态空间。

> 原型 M0 用**穷举 DFS odometer**(`weaveproto/weave.go` 的 `nextPlan`)占位,完备但无剪枝;
> M1 将其替换为 DPOR。二者对外契约(`choose`/`event`)一致,可平滑替换。

## L4:公共 API

新增 `src/testing/weave/weave.go`,镜像 `testing/synctest`:

```go
// 在受控 bubble 内反复运行 f,系统枚举 goroutine 交错,直到覆盖所有(偏序等价类下的)
// 调度或命中上界。任一调度触发 t.Error/panic/死锁/泄漏即报告 + 打印可复现交错 + seed。
func Test(t *testing.T, f func(t *testing.T))

// 阻塞到 bubble 内其余 goroutine 全部 durably blocked;用作收尾或断言中间不变量。
func Wait()
```

内部:`synctest.Run` 起 controlled bubble → 注册 L3 引擎 → for 循环由引擎驱动每条 schedule;
失败用记录的 schedule 前缀确定性重放打印 trace;复用 `testing` 的 tRunner/子测试汇报。

## 失败报告可读性:goroutine 图例

报告以稳定 id `gN`(`weaveWid`,按入组顺序分配)标识 goroutine。为便于阅读,失败/死锁 trace
前打印一段**图例**,把每个 `gN` 映射到它的**创建位置**(`go` 语句的源码点)与所在函数:

```
  goroutines:
    g0: model root
    g1: weavedemo.TestDeadlock.func1 (weave_test.go:79)
    g2: weavedemo.TestDeadlock.func1 (weave_test.go:85)
```

实现:创建位置就是 `go` 语句 PC——`newproc` 里 `sys.GetCallerPC()` 传入 `newproc1` 存进
`newg.gopc`。`weaveAssignWid` 分配 wid 时按 wid 记录 `gp.gopc` 到 `weaveControl.spawnPC`
(driver 提供的输出切片,经 `runSchedule` linkname 带回);`internal/weave.buildGoroutines`
用 `runtime.CallersFrames` 解析为 file:line+func,填入 `Result.Goroutines`;
`testing/weave.formatGoroutines` 渲染图例。g0 是模型根(无派生 `go` 语句),标为 `model root`。
函数名显示的是 `go` 语句的**外层函数**(通常即模型闭包),行号区分不同派生点。

## 失败复现机制

失败时序完全由调度器在各决策点的选择序列(`choices []int`)决定。捕获该向量 → 编码成 seed →
`Replay(seed, f)` 或 `WEAVE_REPLAY=<seed>` 确定性重放同一交错。前提:模型除调度外确定
(无 rand/真实时间/map 迭代序依赖),且改代码 seed 失效。原型已实现,见
`weaveproto/weave.go` 的 `EncodeSeed`/`DecodeSeed`/`Replay`。

## 与 `-race` 的关系(互补,非竞争)

用户常见疑问:weave 与 race 检测器重叠在哪、区别是什么。结论:**互补**。

- **找的 bug 类别不同(最本质)**:
  - race:数据竞态——对同一内存的**无同步并发访问**(至少一写、无 happens-before)。
  - weave:**调度相关的逻辑 bug**——死锁、丢更新、不变量破坏、goroutine 泄漏、原子性违背。
    一段**完全 race-free**(同步都加对了)的代码仍可能丢更新/死锁,race 永远报不出,weave 能。
    反之纯非原子内存竞态 weave 默认不逐指令枚举,交给 race(这正是 L1"内存档"标记为"可选补"的原因)。
- **被动观察 vs 主动控制**:race 被动旁观**这次恰好跑出的那个调度**,buggy 交错没触发就静默漏报
  (false negative 取决于调度运气+输入);weave **接管调度**,串行化+DPOR 系统枚举交错空间,不靠运气,
  且每条交错可用 seed 确定性复现。
- **happens-before / 向量钟——重叠但用途不同(疑似重叠的根源)**:race 用 HB 判"这次访问算不算竞态";
  weave/DPOR 用 HB 判"两 transition 是否独立",从而剪掉偏序等价的交错。二者都用类似 `-race` 的
  编译器插桩(L1 明确类比 race pass),但 race 只建 HB 图**不改调度**,weave 要在同步点**改变调度**。

一句话:race 回答"这一次跑有没有漏同步",weave 回答"在所有合法调度里有没有哪次会算错/死锁/泄漏"。
二者可叠加(weave 把非原子内存竞态委托给 race),覆盖面互补。

## 关键风险

- **只在同步点切换**:非原子内存竞态默认不逐指令枚举(交给 `-race`;L1 内存档可选补)。可行性前提。
- **状态爆炸**:DPOR 主武器 + 抢占上界兜底;`Test` 需报告已探索 schedule 数与是否被截断。
- **runtime 复杂度**:控制器与 GC/抢占/sysmon 交互需谨慎;严格限定仅 controlled bubble 内受影响。
- **不改主调度循环**:控制都在 bubble 分支与 ready/park 边界,降低对 schedule/findRunnable 的风险。
