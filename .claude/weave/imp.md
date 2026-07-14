# weave 实现与状态

> 本文是**实现映射 + 里程碑状态**:每层落在哪个文件/符号、关键运行时机制、门控与成本、边界
> 情况处理、已完成/未完成清单、已知坑。设计理由见 [design.md](design.md);结构总览见
> [arch.md](arch.md)。
>
> ⚠️ 文件行号会随代码演进漂移;本文尽量引用**符号名**而非行号。以当前源码为准。

---

## 1. 现状速览(可用里程碑)

**已达到可用**:未改写的真实 `chan`/`select`/`sync.Mutex`/`RWMutex`/`WaitGroup`/`Cond`/`Once` +
普通内存(`-weave`)代码,可在 `weave.Test` 内被系统性交错探索,失败给出逐步 trace + goroutine
图例 + 可复现 seed。DPOR(含差分健全性验证 + 抢占计数上界)、select-case 枚举、非阻塞 channel
操作、RNG/map 序确定化、spawned goroutine panic 捕获、`go test -weave` flag 均已落地。

**唯一大缺口**:`sync/atomic` 插桩——atomic 目前既非调度点也不记录,用 atomic 的无锁代码探索
不了。正解仿 `-race` 用 instrumented std 重建,工程量大,为免拖累全体 Go 程序 atomic 性能未
草率合入,留作独立专注实现。

---

## 2. 实现映射(文件 / 符号)

### L2 控制器 —— `src/runtime/weave.go`(始终编译,无 build tag)
- **`weaveControl`**:一条调度的调度器状态。runnable 集合用并行定长数组(`runnable[guintptr]` +
  `runnableWid/Op/Addr/PC/Val/ValSet`),`weaveEnqueue`(从 `ready` 调,可能在禁写屏障处)无需
  写屏障、无分配。trace 缓冲 + select 维度 + `plan`/`selPlan` 重放向量 + 存活计数 + done/deadlock
  + 确定性 RNG 状态。
- **令牌交接**:`weaveStart`/`weaveRegisterChild`/`weaveTake`(`weaveChoose` 选下一个:按 plan
  重放或选最小 wid,并记录 transition)/`weaveGrant`/`weaveHandoff`。
- **中心钩子**:`weaveEnqueue`(ready 截获)、`weaveOnBlock`(park_m 交接/死锁)、`weaveOnGoexit`
  (退出交接/完成/死锁)。
- **调度点原语**:`weaveSchedPoint(op, id)`(nosplit,非活跃即返回)→ `weaveSchedPointSlow`;
  内存 hook `weaveread`/`weavewrite`/`weavewriteval`/`weavereadrange`/`weavewriterange`。
- **root 等待**:`weaveRootWait`/`weaveRootPark`(`rootParked` 标志在 park 的 unlockf 里、root 已
  `_Gwaiting` 后才置位,关闭 wake-before-park 竞态)。
- **linkname 面**:`weaveRunSchedule`→`internal/weave.runSchedule`(跑一条调度,带回 steps/nsel/
  outcome/panic)、`weaveYield`→`Yield`、`weaveWait`→`Wait`;全局门控变量 `weaveGloballyActive`。
- **op 码**(与 internal/weave 平行、顺序敏感):None/Read/Write/Lock/Unlock/ChanSend/ChanRecv/
  ChanClose/Select/WaitGroupWait/GoStart/GoExit/Preempt/WaitGroupAdd/CondWait/CondSignal/
  CondBroadcast/Once/**ChanSendNB/ChanRecvNB**(非阻塞,调度点但不建 HB)。
- **panic 捕获**:`weaveGoWrapper` 用 `defer weaveRecoverChild` 把 spawned goroutine 的 panic 记到
  `weaveControl.panicValue`(不崩溃进程)。

### runtime 集成 —— `src/runtime/proc.go`
- `ready`:受控参与者且 `weaveBlocked` 且 `waitreason != waitReasonPreempted` → `weaveEnqueue`
  截获(不 OS-runnable)。排除 `waitReasonPreempted` 是因 GC 栈扫描抢占经 `_Gpreempted`→
  `_Gwaiting`→`ready` 恢复,那不是同步唤醒,参与者仍持令牌须原地恢复(见 design D10 上游修复)。
- `park_m`:受控参与者真实阻塞时,`weaveBlocked=true` 置于 `waitunlockf` **之前**(park 对 waker
  可见之前,关闭竞态,见 D10),park 中止分支回退;park 落定后 `weaveOnBlock` 交接令牌。
- `goexit0`:退出后 `weaveOnGoexit` 交接。
- `newproc`:受控下子 goroutine 建为 parked(在 `weaveGoWrapper` 里,便于捕 panic),注册进控制器,
  父继续持令牌。

### synctest 集成 —— `src/runtime/synctest.go`
- `synctestBubble` 加 `controlled bool` + `weaveCtl *weaveControl`。
- `changegstatus`/`incActive`/`decActive` 对受控 bubble no-op(控制器自管存活,run-token 不变量
  取代空闲计数)。
- `synctestRun1(f, controlled)`:受控分支建 parked main + `weaveStart` + `weaveRootWait`,**跳过
  synctest 假时钟推进循环**(D11 的根源)。

### chan / select —— `src/runtime/chan.go` / `select.go`
- chan:`chansend`/`chanrecv`/`closechan` 在**加锁前**插 `weaveSchedPoint`。阻塞用
  `ChanSend/Recv/Close`;非阻塞(block==false)用 `ChanSendNB/RecvNB`,且**调度点移到非阻塞快速
  失败路径之前**(否则失败的非阻塞 op 直接返回、根本不 yield)。
- select:受控 bubble 内 poll order **确定化**(跳过 `cheaprandn` 洗牌);`weaveSchedPoint(Select)`
  在锁任何 channel 之前;就绪 case **非破坏性 peek**(用 `waitq.weaveHasClaimable` 与 `dequeue` 同一
  有效性判定,**排除已被别的 case 唤醒的 stale sudog**,否则会误报死锁)后由 `weaveSelectChoose`
  选,alternatives 经 selPlan/selTrace 枚举。

### sync 原语 —— `src/sync/*` / `src/internal/sync/*`
- `internal/sync.Mutex.Lock/Unlock/TryLock`、`sync.RWMutex.Lock/RLock/Unlock/RUnlock/TryLock/
  TryRLock`、`WaitGroup.Wait/Add`、`Cond.Wait/Signal/Broadcast`、`Once.Do` 入口经 linkname 调
  `runtime.weaveSchedPoint`(op 对应;Try 系列复用 lock transition),用 `weaveGloballyActive` 做
  **单次全局 load** 门控 → 非 weave 程序几乎零成本。

### 编译器 / cmd/go —— `cmd/compile` / `cmd/go`
- `base.Flag.Weave`;`ir.Syms.Weaveread/Weavewrite/Weavewriteval/Weavereadrange/Weavewriterange`。
- `gc/main.go` 对 `NoInstrument` 包强制关掉 Weave(杜绝 runtime 自插桩递归)。
- `ssagen/ssa.go` 的 `instrument2` 加 weave 分支(复用 race 的 `s.load`/`s.store`/`instrumentFields`/
  `instrumentMove` 插入点);`weaveWriteVal` 把整型/bool 右值零扩展成 uint64 供值显示。
- `cmd/go`:`-weave` flag(`cfg.BuildWeave`)→ `weaveInit`:等价 `-gcflags=-weave`,定义 `weave`
  构建标签,并对 `-race`/`-msan`/`-asan` 互斥报错。

### L3 引擎 —— `src/internal/weave/explore.go`
- `Explore`/`ExploreBudget`/`ExploreBounded`(context bounding + wall-clock 超时)/`Replay`/`Run`。
- `conflict`(含 select addr-agnostic 特判 + NB 处理)、`channelHB`(向量钟,只建 channel 边、
  忽略 NB)、`isChanOp`/`isSyncOp`。
- source-DPOR 主循环:backtrack 集合 + select-case 枚举(`selD`/`selCase`);`exploreExhaustive`
  (odometer)做等价性对拍;`buildTrace`/`buildGoroutines`/`encodeSeed`/`decodeSeed`。

### L4 API —— `src/testing/weave/weave.go`
- `Test(t, f)` / `Yield` / `Wait`;`formatTrace`/`formatGoroutines` 渲染报告;截断报
  `INCOMPLETE` + `t.Errorf`。

### 演示 —— `weavedemo/`(独立 module)
- `weave_test.go`(丢更新/加锁/channel/check-then-act/死锁)、`asyncio_test.go`(异步 IO/
  net.Pipe/context.WithCancel/超时泄漏)、`gcrepro_test.go`(GC 抢占压力,`-weave` 下跳过——见 §7)。
- 跑法:`cd weavedemo && ../bin/go test -weave -v`(部分用例为**故意失败**以演示 weave 抓 bug)。

---

## 3. 关键运行时机制

- **新 goroutine 生下来就 park**:`newproc1(fn, callergp, pc, parked=true, waitReasonWeaveScheduled)`
  → g 建在 `_Gwaiting` 且不入 runq,由控制器 `weaveGrant` 唤醒。普通 `go` 走 `parked=false`+
  `runqput`+`wakep`;受控下改道。
- **切换而不产生并行**:`gopark(unlockf, ...)`,在 unlockf(`weaveHandoff`)里 `weaveGrant(next)`。
  `park_m` 把当前 g 切下 M 之后才调 unlockf,保证 next 只在 self 让出 M 后才可运行。不变量:
  任一时刻只有被选中的 g 可运行,天然串行。
- **控制器 park 不能被误判 durably-blocked**:等令牌的 park 用 `waitReasonWeaveScheduled`
  (不在 idle 表),故不会误降 `bubble.running`;只有真实 durable block 才触发全组静止。
- **run-token 与原型同构**:与 `weavedemo` 原型的协作式调度器同构——原型用 resume/yield channel
  握手,真实版用 `gopark`/`goready` 握手;调度点从库级 `Value`/`Mutex` 换成真实钩子。故 L3 决策
  逻辑可直接从原型移植进 `internal/weave`。

---

## 4. 门控与成本(当前实现)

> 注:早期做过 `weaveenabled` const + `weave_on.go`/`weave_off.go` 的 **build-tag DCE 方案**
> (像 -race),**已废弃**。当前是单个始终编译的 `runtime/weave.go` + 运行时门控(见 design D8)。

- 热路径(chan/select)先判 `weaveActive()`(`gp.bubble != nil && controlled`)——普通程序只多
  一次可预测的 not-taken 分支。
- sync 层用 `weaveGloballyActive`(受控 bubble 存在时才非 0)做单次全局 load,避免每次 `Lock`
  取 `getg().bubble`。
- L1 内存插桩遍布每次访问,故仍需 `-weave` 构建档(与非 weave 构建零影响)。

---

## 5. 边界情况处理

### A. goroutine 不经过调度点(死循环 / 纯计算)
1. **忙等共享变量**(`for !ready {}`):被 L1 内存插桩自然解决——每次读 `ready` 即调度点。
2. **纯计算 / 忙等非共享**:**抢占看门狗**(复用 Go 异步抢占,把被抢占的 g 路由进控制器作强制
   调度点)——**未实现**,列为后续。GC 异步抢占本身已被正确处理(不丢令牌,见 D10)。
3. **真·不终止**:每条 schedule 有调度数/时间预算,超预算标 `Truncated`(把"挂死"转为可定位的
   `INCOMPLETE`,而非静默 ok)。

### B. goroutine 离开受控世界(I/O / 真实时间)
1. **真实时间/定时器**:受控 bubble 内假时钟推进**被禁用**(D11)——`time.Sleep`/`time.After`/
   `NewTimer`/`context.WithTimeout` 会误报死锁。用内存接缝替代(channel/select/`net.Pipe`/
   `context.WithCancel`)。实证见 `weavedemo/asyncio_test.go`。
2. **真实网络/文件 I/O**:须用内存 fake,继承 synctest 规矩。跨 bubble 通信保留 synctest 现有
   `fatal`(`send/recv on synctest channel from outside bubble`)。
3. **泡泡外并发的显式检测**(`uncontrolled concurrency detected`)——**未实现**,roadmap 候选
   (见 design D9)。

---

## 6. 里程碑与状态

> 说明:早期规划编号 M0–M6 与后续实现快照编号不完全一致,下表以**能力**归类,状态以当前代码为准。
> 状态图例:✅ 完成 · 🚧 部分 · ⬜ 未开始。

| 能力 | 状态 | 落点 |
|---|---|---|
| M0 原型引擎(纯 Go,穷举 + 库级插桩) | ✅ | 已完成使命,原型库删除,现保留 `exploreExhaustive` 做对拍 |
| DPOR 替换穷举(source-DPOR + 差分对拍) | ✅ | `explore.go` |
| 抢占计数上界(CHESS context bounding) | ✅ | `ExploreBounded` |
| L2 接入真实 chan/mutex/rwmutex/waitgroup/cond/once(零改写) | ✅ | runtime + sync |
| L1 编译器内存插桩(`-weave`,含结构体/切片/值显示) | ✅ | cmd/compile + cmd/go |
| channel happens-before(DPOR 对 channel 冲突健全) | ✅ | `channelHB` |
| select 确定化 + 多就绪 case 枚举 | ✅ | select.go + `selD/selCase` |
| select vs 并发 chan 的冲突约简(addr-agnostic) | ✅ | `conflict` 特判 |
| 非阻塞 channel 操作作为调度点(no-HB) | ✅ | ChanSendNB/RecvNB |
| RNG / map 迭代序确定化 | ✅ | `weaveRand`,select.go/rand.go |
| spawned goroutine panic 捕获 | ✅ | `weaveGoWrapper` |
| 失败 seed 重放 + 源码行号 + goroutine 图例 + 读写值 | ✅ | explore + testing/weave |
| 探索预算 / wall-clock 超时 / 截断显式上报 | ✅ | `ExploreBounded`/`Truncated` |
| `-weave` 与 `-race`/`-msan`/`-asan` 互斥 | ✅ | `weaveInit` + compile 校验 |
| **atomic 插桩** | ⬜ | 最大缺口,见 §1 |
| 弱内存模型(atomic C11 重排 / read-from)| ⬜ | 依赖 atomic |
| optimal-DPOR(wakeup tree)进一步剪枝 | ⬜ | 增强 |
| 抢占看门狗(死循环兜底)| ⬜ | 见 §5.A.2 |
| 泡泡外并发显式检测 + undo-log 跨 run 重置 | ⬜ | 见 design D9 |
| 并行探索(多 worker 跑子树)、状态空间估计 | ⬜ | UX |
| 受控 bubble 内假时钟推进 | ⬜ | 已知限制 D11 |
| 死锁泄漏清理 | ⬜ | 已知限制(与 synctest 一致,可接受)|

---

## 7. 已知坑 / 注意事项

- **快速路径钩子要极轻**:非受控 bubble 时必须一次判空即返回(nosplit 友好)。
- **控制器自身不能递归触发 weaveSchedPoint**:driver / 非参与者的内存访问是 no-op(runtime 从不
  被插桩)。
- **GC/finalizer/系统 goroutine 不入 bubble**,天然不受影响;但 GC **抢占**会同步打断持令牌的
  参与者——已正确处理(D10:排除 `waitReasonPreempted`,`weaveBlocked` 前置)。
- **假时钟禁用**(D11):`time.Sleep`/定时器误报死锁,用内存接缝。
- **`-weave` 下重内存/热循环用例状态空间爆炸**:如 `weavedemo/gcrepro_test.go` 的 GC 抢占压力
  测试(200 轮 × 长 spin),`-weave` 下每次内存访问成调度点会撑到超时。它测的是令牌在 GC 抢占下
  的健壮性、非数据竞争,故用 `weave` 构建标签跳过、并 `-short` 降迭代。
- **非确定性来源**(模型需规避):GC/finalizer/`AddCleanup` 时序、`sync.Pool`、指针地址(ASLR/
  分配器/裸指针 hash)、cgo/外部进程——均不可控,模型不得依赖。

---

## 8. 分步验证方法(降低 runtime 改错风险)

1. **只加字段 + no-op 钩子**,跑 `go test runtime sync internal/synctest testing/synctest` 确认零回归。
2. **round-robin 串行化**,验证真实 `go func()` 严格一次跑一个。
3. **接真实 sync.Mutex**(internal/sync 入口加 schedPoint),验证真实 Mutex 用例可探索。
4. **接 chan/select**,移植 L3 引擎,`testing/weave.Test` 可用。
5. **失败 seed 重放**;差分健全性对拍(`TestDPORSoundnessSuite`)。

**日常构建/测试**:
```sh
cd src && ../bin/go test internal/weave/ testing/weave/       # 核心引擎(无 -weave)
cd src && ../bin/go test -weave internal/weave/                # 含内存插桩的健全性套件
cd src && ../bin/go test runtime sync internal/synctest testing/synctest   # 回归
```
工具链按需重编改动的 std 包,纯 Go 编辑无需 `make.bash`。

---

## 9. 评审驱动的修复(2026-07)

一轮针对 head 的代码评审(PR #1)报出并已修复的一组健全性/正确性/健壮性缺口,每条都配了回归测试:

**健全性(漏状态/假死锁)**
- **内存冲突按区间重叠 + 记录 width**:range 钩子曾丢 `size`、`conflict` 只比地址相等 → 整struct写
  vs 字段读被判独立而漏状态。size 现贯穿 hook→trace→`runSchedule`,`conflict` 按 `[addr,addr+size)`
  重叠判定。回归 `TestStructWholeWriteVsFieldRead`。
- **channelHB 不产生假边**:成功的 NB recv / select-recv 消费值但 FIFO 不 pop,后续普通 recv 会
  pop 到过期 send clock → 假 HB 剪掉真实反转。现 taint NB 触及的 channel、含 select 的 run 禁用
  channel HB。回归 `TestChannelHBNoStaleEdge`。
- **select 就绪 peek 排除 stale sudog**:`waitq.weaveHasClaimable` 与 `dequeue` 同一有效性判定,
  不再把已被别的 case 唤醒的 sudog 当就绪 → 不再误报死锁。回归 `TestSelectStaleSudogNoFalseDeadlock`。

**正确性/保证**
- **抢占上界约束实际执行的 schedule**:bounded 模式默认后缀 sticky(`weaveControl.stickyDefault`),
  非强制后缀零抢占,故不会执行/上报越界后缀产生的 failure。回归 `TestPreemptionBoundConstrainsExecuted`。
- **溢出先判 outcome==2 再切片 + clamp**:65536+ 调度点不再 `slice out of range` panic,如实报
  `Truncated`。回归 `TestTraceOverflowTruncates`。
- **预算恰好覆盖全空间不误标 Truncated**:仅当确有下一条 schedule 时才判预算。回归
  `TestBudgetExactCoverageNotTruncated`。
- **TryLock 系列补调度点**:`Mutex.TryLock`、`RWMutex.TryLock/TryRLock` 现为调度点(复用 lock
  transition),兑现"mutex 操作皆调度点"。回归 `TestTryLockExplored`。

**健壮性/工程**
- **容量耗尽优雅截断**:`weavePush` 超 `weaveMaxRunnable` 改为设 overflow + 返回 outcome 2(优先于
  deadlock 判定),不再 `throw` 杀进程。回归 `TestTooManyGoroutinesTruncates`。
- **读值在授予令牌时采样**:`weaveChoose` 对 read op 重新 `weaveLoadVal`,trace 显示参与者真正读到的
  值(而非 park 前的旧值)。回归 `TestReadValueReflectsGrantTime`。
- **`-weave` 不覆盖用户 `-gcflags`**:改用 `forcedGcflags`(附加、不 shadow 用户规则)。回归
  `cmd/go` script `build_weave.txt`。
- **replay 命令 shell-quote + 保留构建模式**:seed 单引号包裹(select seed 含 `|`)、`-run` 锚定、
  仅在 `-weave` 构建下带 `-weave`。回归 `testing/weave.TestReplayCommandFormat`。
- **wall-clock timeout 明确为 runs 之间的 soft limit**(单条卡死 schedule 需外部 `-timeout`;
  抢占看门狗仍是已知缺口,见 §5.A.2)。

**Go 仓库集成门禁**
- `testing/weave` 登记进 `api/next/weave.txt` 与 `go/build/deps_test.go`;`cmd/api TestCheck` 与
  `go/build.TestDependencies` 通过。
