# weave 实现与状态

> 本文是**实现映射 + 里程碑状态**:每层落在哪个文件/符号、关键运行时机制、门控与成本、边界
> 情况处理、已完成/未完成清单、已知坑。设计理由见 [design.md](design.md);结构总览见
> [arch.md](arch.md)。
>
> ⚠️ 文件行号会随代码演进漂移;本文尽量引用**符号名**而非行号。以当前源码为准。

---

## 1. 现状速览(可用里程碑)

**已达到可用**:未改写的真实 `chan`/`select`/`sync.Mutex`/`RWMutex`/`WaitGroup`/`Cond`/`Once`/
`sync/atomic`(类型化 API)+ 普通内存(`-weave`)代码,可在 `synctest.Test`(`-weave`)内被系统性交错探索,
失败给出逐步 trace + goroutine 图例 + 可复现 seed。DPOR(含差分健全性验证 + 抢占计数上界)、select-case
枚举、非阻塞 channel 操作、假时钟推进、RNG/map 序确定化、spawned goroutine panic 捕获、`go test -weave`
flag 均已落地。

**易用性增强**:
- **默认自动迭代加深抢占上界**:不设 `WEAVE_MAX_PREEMPTIONS` 时,从抢占=0 逐层加深到默认上限
  `defaultPreemptCeiling`(=2,`weaveExplore`/`exploreIterative`),而非旧的无界搜索——避免真实模型状态
  空间爆炸,并让反例用最少抢占(最易读)。设了该 env 则以其为上限。
- **每个 transition 都带源码位置**(D23):不只内存读写,`lock`/`chan send`/`once`/`atomic` 等也指到
  **用户写的那一行**(库级钩子跳一帧,由 `runtime.weaveSchedPointSkip` 在确认是参与者后才 unwind)。
- **跨 schedule 残留状态会被指出**(D23):模型读到前一遍写的值时,报告附上**读点与写点**两个 `file:line`;
  只写不读的外部状态一声不响。报出反例前还会用同一条选择向量**复现一次**,不复现就不报。
- **失败报告可读性**:轨迹里同步对象由裸地址改为稳定标签(`mutex#1`/`chan#2`/`cond#3`/`wg`/`once`/`mem`,
  `addrLabeler`);死锁报告额外逐 goroutine 列出等待对象(`gN blocked on lock mutex#4`,由每个参与者的
  最后 trace step 推断,`blockedWaits`)。
- **假时钟推进本身是可枚举的调度选项**(D22):有 pending timer 且**有任一参与者阻塞**时,"推进到最近
  deadline 并触发定时器"作为伪参与者(`weaveClockWid`)参与选择,于是"超时 vs 常驻可运行事件"能被探到。
  选它只要还有其他参与者可运行就计一次抢占,故 `WEAVE_MAX_PREEMPTIONS=0` 精确等于 synctest 的 idle-only
  契约。`Result.UnfiredTimer` 的提示改为**仅当任何 schedule 都未推进过时钟**时才打印(`ClockAdvanced`)。

**`sync/atomic` 插桩(已部分落地,见 D18)**:`sync/atomic` 的**类型化 API**(`atomic.Int64/Uint64/
Int32/Uint32/Uintptr/Bool/Pointer` 的 `Load/Store/Swap/Add/CompareAndSwap/And/Or`)现已成为调度点——在
`type.go` 方法里调 `weaveAtomic`(库级钩子,仿 `sync.Mutex`),故 `atomic.Load()+Store()` 之类非原子 RMW 能被
探索。**尚缺**:自由函数(`atomic.LoadInt64` 等,是编译器 intrinsic,须仿 `-race` 在 `-weave` 下关掉 intrinsic
再走带钩子实现)与 `atomic.Value`——留作后续。

**库级钩子的成本模型(D20)**:钩子的**作用域分两档**,别记混:
- **始终编译 + 运行期门控**(不带 `-weave` 也是调度点):runtime 侧的 chan/select(D8,`runtime/weave.go` 无
  build tag)+ `internal/sync.Mutex` 的 `Lock/TryLock/Unlock`。
- **仅 `-weave`**(`weaveEnabled` 编译期常量 + build tag 分流,普通构建成本精确为零):`sync` 包的 `RWMutex`
  (含读锁与写锁)、`Once`、`Cond`、`WaitGroup`,以及 `sync/atomic`。
钩子函数体一律 `//go:noinline`,否则会被内联进被插桩的命令行包、连门控读都变成调度点(实测每个 atomic 操作
多两个 `read`)。这一改把 `cmd/compile/internal/test.TestIntendedInlining` 长期存在的 3 个失败
(`RWMutex.RLock`/`RUnlock`/`Once.Do` cost 超预算)彻底修掉。`RWMutex` 读锁另有独立 op
`rlock`/`runlock`(报告里标签 `rwmutex#N`)。

---

## 2. 实现映射(文件 / 符号)

### L2 控制器 —— `src/runtime/weave.go`(始终编译,无 build tag)
- **`weaveControl`**:一条调度的调度器状态。runnable 集合用并行定长数组(`runnable[guintptr]` +
  `runnableWid/Op/Addr/PC/Val/ValSet`),`weaveEnqueue`(从 `ready` 调,可能在禁写屏障处)无需
  写屏障、无分配。trace 缓冲 + select 维度 + `plan`/`selPlan` 重放向量 + 存活计数 + done/deadlock
  + 确定性 RNG 状态。
- **令牌交接**:`weaveStart`/`weaveRegisterChild`/`weaveTake`(`weaveChoose` 选下一个:按 plan
  重放或选最小 wid,并记录 transition)/`weaveGrant`/`weaveHandoff`。`weaveTake` 还会经
  `weavePushClock` 把"推进假时钟"作为伪参与者放进候选集;若它被选中,当前参与者经 `weaveHandoffClock`
  把令牌交给 root 去推进(D22)。
- **中心钩子**:`weaveEnqueue`(ready 截获)、`weaveOnBlock`(park_m 交接/死锁)、`weaveOnGoexit`
  (退出交接/完成/死锁)。
- **调度点原语**:`weaveSchedPoint(op, id)`(nosplit,非活跃即返回)→ `weaveSchedPointSlow`;带源码
  位置的两个变体(D23):`weaveSchedPointAt`(调用方已知 caller PC,chan/select 用)与
  `weaveSchedPointSkip`(库级钩子用,确认是参与者后才 unwind 指定帧数)。内存 hook
  `weaveread`/`weavewrite`/`weavewriteval`/`weavereadrange`/`weavewriterange`。
- **root 等待**:`weaveRootWait`/`weaveRootPark`(`rootParked` 标志在 park 的 unlockf 里、root 已
  `_Gwaiting` 后才置位,关闭 wake-before-park 竞态)。
- **linkname 面**:`weaveRunSchedule`→`internal/weave.runSchedule`(跑一条调度,带回 steps/nsel/
  outcome/panic)、`weaveYield`→`Yield`、`weaveWait`→`Wait`;全局门控变量 `weaveGloballyActive`。
- **op 码**(与 internal/weave 平行、顺序敏感):None/Read/Write/Lock/Unlock/ChanSend/ChanRecv/
  ChanClose/Select/WaitGroupWait/GoStart/GoExit/Preempt/WaitGroupAdd/CondWait/CondSignal/
  CondBroadcast/Once/**ChanSendNB/ChanRecvNB**(非阻塞,调度点但不建 HB)/AtomicLoad/AtomicStore/
  AtomicRMW/**RLock/RUnlock**(RWMutex 读锁)/**ClockAdvance**(假时钟推进,由伪参与者 `weaveClockWid`
  承载,见 D22)。五处声明由 `internal/weave/optab_test.go` 交叉校验。
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
- 受控 bubble **不由 `synctestRun` 建**:`weaveRunBubble`(runtime/weave.go)自己建 bubble、把 f 作为
  parked 主参与者 + `weaveStart` + `weaveRootWait`,因为它要挂上每条 schedule 各自的 `weaveControl`。
  `weaveRootWait` 就是 weave 版的假时钟推进循环(见 §5.B.1 与 design D11/D22);普通 synctest 的静止
  循环在 `synctestRun` 里,两者互不相干。

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
  `runtime.weaveSchedPoint`(op 对应;`Try` 系列复用对应的 lock transition,读锁用 `rlock`/`runlock`)。
- 门控分两档(见 §1 与 design D20):`internal/sync/weave.go` 是**始终编译**的运行期门控
  (`weaveGloballyActive` 单次全局 load);`sync/weave.go` + `weave_on.go`/`weave_off.go` 是**编译期常量**
  `weaveEnabled` 门控,普通构建里整段是死代码。两处的钩子体都 `//go:noinline`。

### 编译器 / cmd/go —— `cmd/compile` / `cmd/go`
- `base.Flag.Weave`;`ir.Syms.Weaveread/Weavewrite/Weavewriteval/Weavereadrange/Weavewriterange`。
- `gc/main.go` 对 `NoInstrument` 包强制关掉 Weave(杜绝 runtime 自插桩递归)。
- `ssagen/ssa.go` 的 `instrument2` 加 weave 分支(复用 race 的 `s.load`/`s.store`/`instrumentFields`/
  `instrumentMove` 插入点);`weaveWriteVal` 把整型/bool 右值零扩展成 uint64 供值显示。
- `cmd/go`:`-weave` flag(`cfg.BuildWeave`)→ `weaveInit`:定义 `weave` 构建标签,并对
  `-race`/`-msan`/`-asan` 互斥报错。**注意作用域**:`-weave` 这个 gcflag 只加给**命令行包**
  (`work/gc.go` 里 `cfg.BuildWeave && p.Internal.CmdlinePkg`,附加而非 shadow 用户 `-gcflags`),依赖包
  **不插桩**——`cmd/go/testdata/script/build_weave.txt` 断言了 `-p sync` 不带 `-weave`。但 build tag 是
  **全构建生效**的,所以库级钩子(atomic/sync)按 tag 在所有包里生效;且**可内联的依赖函数体会随内联被搬进
  命令行包一起插桩**,这就是钩子体必须 `//go:noinline` 的原因(D20)。

### L3 引擎 —— `src/internal/weave/explore.go`
- `Explore`/`ExploreBudget`/`ExploreBounded`(context bounding + wall-clock 超时)/`Replay`/`Run`。
- 跨 run 一致性(D23):`checkPrefix`(重放分歧,含"默认调度跑三遍"的两级探针)、`checkCarriedState`
  (首读值证据)、`confirm`(报出反例前复现一次);产物是 `Result` 的 `Diverged`/`WarmupDiverged`/
  `CarriedState`/`NotConfirmed` 四组字段,**除 Replay 的失配外都只作证据**。
- `conflict`(含 select 与 clock-advance 的 addr-agnostic 特判 + NB 处理)、`channelHB`(向量钟,
  只建 channel 边、忽略 NB)、`isChanOp`/`isSyncOp`。
- source-DPOR 主循环:backtrack 集合 + select-case 枚举(`selD`/`selCase`);`exploreExhaustive`
  (odometer)做等价性对拍;`buildTrace`/`buildGoroutines`/`encodeSeed`/`decodeSeed`。

### L4 API —— `src/testing/synctest/`(标准库,无 weave 专属包)
- `synctest.Test(t, f)` / `synctest.Wait`——`-weave` 下由 `weave.go`(`//go:build weave`)的
  `weaveExplore` 改派到 L3;`weave_off.go`(`//go:build !weave`)是 no-op(普通单跑)。`formatTrace`/
  `formatGoroutines`/`replayCommand` 等富报告渲染现居于 `testing/synctest/weave.go`;截断报 `INCOMPLETE`。
- `testing.testingWeaveTest`(linkname 到 testing/synctest):把 f **内联作为 bubble 的主参与者**跑
  (即 wid 0,不是 root/driver),并把 `t.Fatal/Error`/panic 失败转成 panic 供引擎捕获(不用
  `testingSynctestTest` 的子 goroutine,避免幽灵 root 等待者导致的伪死锁 + 模型放大);`t.Skip` 走
  `weaveTestSkip` 哨兵转成真正的跳过(D21)。
- `runtime.synctestWait`:受控 bubble(`bubble.controlled`)内路由到 `weaveWait`。

### 演示 —— `weavedemo/`(独立 module)
- **根目录不放 `.go` 文件**(2026-07-29 重组):全部用例都在 `supported/` 与 `unsupported/` 两个子 package 里,
  各自有 `doc_test.go` 说明该目录的契约。
- `supported/`(一用例一文件)——**weave 处理正确**的用例,两类,文件头都写明属于哪一类:
  - **`-weave` 下预期 FAIL**:weave 能稳定发现的真实并发 bug(确定失败并给可复现 seed;plain 单跑行为不定
    ——可能漏报、确定命中或 flaky,这正是 weave 相对单跑的价值。用例按自身合理性编写,**不为迎合单跑调度而改**)。
  - **预期 PASS**:`correct_*.go` 是正确代码/上面某个 bug 的修复版,weave 穷举后不报任何东西——防假阳性的
    对照组(`correct_sync_basics`、`correct_asyncio`、`correct_netfake` 三个文件,后者附"怎么用 net.Pipe 造
    可探索的假传输"的指南);`gcpreempt_test.go` 是 GC 抢占压力测试,`-weave` 下跳过(见 §7)。
- **plain 模式的一个坑**:`supported/timeout_leaks_worker_test.go` 在单跑时必然命中泄漏,而"bubble 死锁"在
  synctest 里是 **panic** 而非测试失败 → 整个测试二进制中止,声明在它之后的用例都不再运行。这恰好说明 weave 的
  价值(同一个 bug 被报成带 trace 和 seed 的失败)。要在 plain 模式跑完整包,用 `-run` 排除它。
- `-weave` 下预期失败的用例清单:
  `double_close_guard_race`(TOCTOU→panic 捕获)、`transfer_inconsistent_read`(跨双锁原子性违背)、
  `cond_lost_wakeup`(`sync.Cond` 丢唤醒→死锁)、`channel_order_assumption`(buffered channel 发送乱序)、
  `rwmutex_misuse_lost_update`(用 RLock 保护写→丢更新)、`select_priority_assumption`(select 无优先级,
  两 case 就绪时走错分支)、`multi_producer_append`(无锁 append 丢元素)、`rwmutex_writer_starvation`
  (持 RLock 再取 RLock,中间写者排队→自死锁)、`lockfree_stack_lost_push`(无锁栈用 Store 代替 CAS 丢节点;
  **weave 靠节点 `next` 的普通字段写这个调度点抓到**——反衬 atomic 盲区仅限"纯 atomic 无普通内存穿插")、
  `channel_lock_deadlock`(**用 buffered channel 当锁的 AB/BA 死锁**;曾经漏报,现由 channelHB 的 token-recycling
  taint 修复后能抓到——见 design.md D16)、`semaphore_oversubscribe`(check-then-act 并发限流器超发)、
  `waitgroup_add_in_goroutine`(`wg.Add` 在 goroutine 内→Wait 提前返回;`go vet` 也能静态抓,weave 动态给轨迹)、
  `atomic_lost_update`(`atomic.Load()+Store()` 非原子 RMW;曾经漏报,现由类型化 atomic 插桩后能抓到——见 D18)、
  `struct_tearing`(整体结构体赋值 vs 读,不变量 x==y;曾经漏报,现由 `-weave` 下多字段结构体逐字段存储后能抓到
  撕裂读 {5,0}——见 D19)、`slice_index_oob`(并发 `s=s[:1]` vs `s[2]`:slice 头竞争→越界 panic 捕获,全新失败类别)、
  `dining_philosophers`(3 方环形等待死锁,区别于 2 方 AB/BA)、`semaphore_leak_deadlock`(取消路径提前 return
  漏还信号量额度→后续 acquire 永久阻塞死锁)、`context_cancel_missed`(轮询 `ctx.Err()` 后再阻塞的 TOCTOU→漏掉
  中途取消→worker 泄漏;演示应 `select` 于 `ctx.Done()`)、`inconsistent_locking`(两处用**不同**的 mutex 保护同一
  变量→无真正互斥→丢更新;演示"加锁≠线程安全,须同一把锁")、以及 2026-07-29 从根目录移入的四个:
  `lost_update`(最朴素的 `x = x+1` 丢更新)、`check_then_act_overdraw`(余额 check-then-act 透支)、
  `mutex_abba_deadlock`(真 `sync.Mutex` 的 AB/BA,对照 channel 版与哲学家版)、
  `timeout_leaks_worker`(超时分支放弃无缓冲 result → worker 永久阻塞泄漏,见上面的 plain 模式坑)。
  共 24 个(2026-07-29 起含 `timer_vs_event`——真 `time.After` 版的超时泄漏,D22 之前是 `unsupported/` 里的假阴性)。
- `unsupported/`(子 package,一用例一文件)——**weave 当前发现不了**的真实 bug(假阴性,`-weave` 下 PASS):
  `weak_memory_publication`
  (无同步发布可见性,弱内存重排未建模,仅 SC+程序序)、`benign_data_race`(良性竞争:weave 只报算错/死锁/
  泄漏/panic,不报缺同步,故 PASS,须靠 `-race` 互补)、`map_concurrent_write`(并发写同一 map;`mapassign`
  是无调度点的 runtime 调用→weave 不交错、漏报,与 atomic 缺口同类;因串行化不会真触发 fatal,`-race` 能抓)、
  `double_checked_locking`(DCL:普通变量做
  fast-path 检查,发布指针先于字段写对读者可见→半构造对象;弱内存重排未建模,weave SC 视角认为正确故 PASS)。
- 跑法:`cd weavedemo && ../bin/go test -weave -v ./...`(`supported/` 里 24 个用例**故意失败**以演示 weave
  抓 bug、`correct_*` 与 `gcpreempt` 预期 PASS;`unsupported/` 全 PASS 作为边界文档)。
- **自动校验:`cd weavedemo && ./check.sh`**。因为用例是"预期失败/预期通过"混编,`go test` 的退出码判不了对错,
  于是脚本**从每个用例的注释里推导期望**(doc comment 含 `EXPECTED TO FAIL` 即必须失败,其余必须 PASS/SKIP),
  跑一遍 `-weave` 再逐个 diff。改动引擎后跑它:某个用例"不再失败"就意味着 weave 丢掉了发现那个 bug 的能力
  ——这是 D16/D18/D19 这类覆盖唯一的自动守卫(weavedemo 是独立 module,不在任何 std 测试里)。当前 37 个用例全对。

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

三档,越靠外越便宜(设计理由见 design D8 + **D20**):

- **runtime 热路径(chan/select)**:`weaveActive()`(`gp.bubble != nil && controlled && gp != root`)
  ——始终编译,普通程序只多一次可预测的 not-taken 分支。
- **`internal/sync.Mutex`**:`weaveGloballyActive`(受控 bubble 存在时才非 0)做单次全局 load,避免
  每次 `Lock` 取 `getg().bubble`。**始终编译**,所以不带 `-weave` 也能探索 mutex 交错(`internal/weave`
  的引擎自测依赖这一点)。
- **`sync` 的 RWMutex/Once/Cond/WaitGroup + `sync/atomic`**:build-tag 常量 `weaveEnabled`
  (`weave_on.go`/`weave_off.go`)门控,普通构建里整段是死代码,成本**精确为零**;钩子体一律
  `//go:noinline`,防止被内联进被插桩的命令行包后连门控读都变成调度点。
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
1. **真实时间/定时器**:受控 bubble 内**已支持假时钟推进**(D11)——`weaveRootWait` 推进 `bubble.now`、
   `timers.check` 触发到期定时器(经 `ready`→`weaveEnqueue` 唤醒参与者)。
   `time.Sleep`/`time.After`/`NewTimer`/`context.WithTimeout` 均可正常工作。**推进时机本身是可枚举的
   调度选择**(D22):有 pending timer 且有任一参与者阻塞时,`weavePushClock` 把它作为候选放进 runnable
   集;无人可运行时它是唯一候选,退化成 D11 的行为。回归 `internal/weave` 的 `TestFakeClock*` 与
   `TestClockAdvance*`。小限制:同 deadline 定时器触发顺序不单独枚举;永不停止的 `time.Tick` 无限推进
   时钟 → 受预算约束(Truncated)。
2. **真实网络/文件 I/O**:须用内存 fake,继承 synctest 规矩。跨 bubble 通信保留 synctest 现有
   `fatal`(`send/recv on synctest channel from outside bubble`)。
3. **泡泡外并发的显式检测**(`uncontrolled concurrency detected`)——**未实现**,roadmap 候选
   (见 design D9)。

---

## 6. 里程碑与状态

> 说明:早期规划编号 M0–M6 与后续实现快照编号不完全一致,下表以**能力**归类,状态以当前代码为准。
> 状态图例:✅ 完成 · 🚧 部分 · ⬜ 未开始 · ❌ 评估后否决。

| 能力 | 状态 | 落点 |
|---|---|---|
| M0 原型引擎(纯 Go,穷举 + 库级插桩) | ✅ | 已完成使命,原型库删除,现保留 `exploreExhaustive` 做对拍 |
| DPOR 替换穷举(source-DPOR + 差分对拍) | ✅ | `explore.go` |
| 抢占计数上界(CHESS context bounding) | ✅ | `ExploreBounded` |
| 默认自动迭代加深抢占上界(未设 env 时 0→2 逐层,替代无界) | ✅ | `exploreIterative`/`defaultPreemptCeiling` |
| 失败报告对象标签化(mutex#/chan#/…)+ 死锁列出各 goroutine 等待对象 | ✅ | `addrLabeler`/`blockedWaits` |
| 假时钟推进作为可枚举调度选项(超时 vs 事件)| ✅ | 伪参与者 `weaveClockWid` + `opClockAdvance`(D22) |
| 未推进过时钟时的假阴性提示 | ✅ | `Result.UnfiredTimer` + `ClockAdvanced` |
| L2 接入真实 chan/mutex/rwmutex/waitgroup/cond/once(零改写) | ✅ | runtime + sync |
| L1 编译器内存插桩(`-weave`,含结构体/切片/值显示) | ✅ | cmd/compile + cmd/go |
| channel happens-before(DPOR 对 channel 冲突健全) | ✅ | `channelHB` |
| select 确定化 + 多就绪 case 枚举 | ✅ | select.go + `selD/selCase` |
| select vs 并发 chan 的冲突约简(addr-agnostic) | ✅ | `conflict` 特判 |
| 非阻塞 channel 操作作为调度点(no-HB) | ✅ | ChanSendNB/RecvNB |
| RNG / map 迭代序确定化 | ✅ | `weaveRand`,select.go/rand.go |
| spawned goroutine panic 捕获 | ✅ | `weaveGoWrapper` |
| 失败 seed 重放 + 源码行号 + goroutine 图例 + 读写值 | ✅ | explore + testing/synctest |
| 探索预算 / wall-clock 超时 / 截断显式上报 | ✅ | `ExploreBounded`/`Truncated` |
| `-weave` 与 `-race`/`-msan`/`-asan` 互斥 | ✅ | `weaveInit` + compile 校验 |
| **atomic 插桩(类型化 API)** | ✅ | `sync/atomic` type.go 方法钩子(D18);自由函数/`atomic.Value` 待做 |
| 库级钩子零成本化 + 修掉内联回归 | ✅ | `weaveEnabled` 编译期门控 + 钩子体 noinline(D20);`TestIntendedInlining` 转绿 |
| `t.Skip` 在探索模式下正确跳过 | ✅ | `weaveTestSkip` 哨兵(D21) |
| op 码跨 5 处声明的交叉校验 | ✅ | `internal/weave/optab_test.go` |
| 演示用例行为的自动断言 | ✅ | `weavedemo/check.sh`(期望值从注释推导) |
| 跨 schedule 残留状态的检测(非回滚)| ✅ | 重放分歧 + 首读值证据 + 失败复现确认(D23) |
| 所有 op 都带源码位置 | ✅ | `weaveSchedPointAt`/`weaveSchedPointSkip`(D23) |
| 弱内存模型(atomic C11 重排 / read-from)| ⬜ | 依赖 atomic |
| optimal-DPOR(wakeup tree)进一步剪枝 | ⬜ | 增强 |
| 抢占看门狗(死循环兜底)| ⬜ | 见 §5.A.2 |
| 自动包装任意测试(免 `synctest.Test`)| ❌ | **已评估并否决**,见 design §9 |
| 泡泡外并发显式检测 + undo-log 跨 run 重置 | ⬜ | 见 design D9 |
| 并行探索(多 worker 跑子树)、状态空间估计 | ⬜ | UX |
| 受控 bubble 内假时钟推进(time.Sleep/After/timer/context.WithTimeout)| ✅ | `weaveRootWait`;`TestFakeClock*` |
| 死锁泄漏清理 | ⬜ | 已知限制(与 synctest 一致,可接受)|

---

## 7. 已知坑 / 注意事项

- **持锁 / procPin 区内不得插调度点(已修,2026-07-16)**:`weaveSchedPointSlow` 若在 `gp.m.locks != 0`
  时 `gopark`,会命中 `fatal error: schedule: holding locks`(schedule() 断言 m.locks==0)。触发场景:
  被插桩的普通内存读写恰好落在一段 procPin / 持 runtime 锁的区间里——实测把整条 **gin `ServeHTTP`**
  请求路径(Context `sync.Pool` 等)并发包进 `synctest.Test`(`-weave`)时必现。修复:`weaveSchedPointSlow` 开头
  `if gp.m.locks != 0 { return }` 直接跳过调度点。**这是健全的**:持锁/pin 区在真实调度器下本就不可抢占,
  weave 不在其中交错正好符合真实语义(少探这一点=更贴近现实,非漏报意义上的少边)。修后回归
  `internal/weave` + `-weave` 套件全绿;之前崩溃的 gin 并发用例可探 4753 调度通过。


- **快速路径钩子要极轻**:非受控 bubble 时必须一次判空即返回(nosplit 友好)。
- **控制器自身不能递归触发 weaveSchedPoint**:driver / 非参与者的内存访问是 no-op(runtime 从不
  被插桩)。
- **GC/finalizer/系统 goroutine 不入 bubble**,天然不受影响;但 GC **抢占**会同步打断持令牌的
  参与者——已正确处理(D10:排除 `waitReasonPreempted`,`weaveBlocked` 前置)。
- **假时钟已支持**(D11):`time.Sleep`/`time.After`/`NewTimer`/`context.WithTimeout` 在受控 bubble
  内正常推进;仅同 deadline 定时器触发序不枚举、永不停止的 ticker 受预算约束。
- **假时钟基准时间已修**(2026-07-22):weave 路径的 `weaveRunBubble` 曾漏设 `bubble.now`,导致 `-weave`
  下 `time.Now()` 返回 1970 而非普通 synctest 的 2000 基准(`TestNow` 失败)。修复:把 `synctestBaseTime`
  提为 runtime 包级 const,`weaveRunBubble` 建 bubble 时一并设 `now`,与普通 synctest 完全一致。
- **"超时 vs 事件"竞争已可探(D22)**:`-weave` 下推进假时钟是一个可枚举的调度选项(有 pending timer 且
  有任一参与者阻塞时提供),所以不必再把事件"对齐到定时器边界"。注意两点:选择时钟计一次抢占,故默认
  迭代加深要到第 1 层才会命中,`WEAVE_MAX_PREEMPTIONS=0` 则退回 idle-only;**不加 `-weave` 时 synctest
  行为不变**(放宽只在受控 bubble 内)。仅同一 deadline 上多个定时器的相对触发顺序仍不枚举。
- **`-weave` 下不适用的测试用 `underWeave` 跳过**:`testing/synctest` 里验证 testing-package **交互
  输出**的用例(TestFatal/Error/VerboseError/Skip/VerboseSkip/Helper——经 `runTest` fork 子进程断言
  非-weave 输出格式;TestContext——用闭包外共享状态断言 `t.Context()` 生命周期)以及重量级/压力用例
  (TestHTTPTransport100Continue 的 net/http 集成、TestSynctestTimerRaceCtxCrash 的 100 定时器压力)
  在探索模式下语义不适用或超容量。它们靠 build-tag 常量 `underWeave`(`underweave_on_test.go` /
  `underweave_off_test.go`)在 `-weave` 时 `t.Skip`——如同标准库对 `-race` 用 `//go:build !race`。
  修后 `-weave` 下 `testing/synctest` 全绿。
- **`-weave` 下重内存/热循环用例状态空间爆炸**:如 `weavedemo/supported/gcpreempt_test.go` 的 GC 抢占压力
  测试(200 轮 × 长 spin),`-weave` 下每次内存访问成调度点会撑到超时。它测的是令牌在 GC 抢占下
  的健壮性、非数据竞争,故用 `weave` 构建标签跳过、并 `-short` 降迭代。同理,包住重插桩库代码
  (msgpack 编解码、crypto/hash 等)的多个 goroutine 若同时可运行,也会爆预算;实测经验是**一次只
  让一个后台 goroutine 活跃**(对端阻塞在 channel 上、让重活单线程跑),可把调度数压回可探范围。
- **map / runtime 调用不产生调度点 → 无锁 map 竞态漏报**:调度点只在被插桩的标量/结构体字段
  load/store、channel、sync 原语处产生;**经 runtime 函数完成的访问(Go map `mapaccess`/`mapdelete`
  等)不插桩、不成调度点**。故一个原子性违背若两个竞争访问**都落在 map 上**(无锁 check-then-act),
  weave 会把整段当原子块 → **漏报**。对拍已证实(2026-07-16,ganc):同一 check-then-act 写在标量
  bool 上 8 个调度即报出,写在 map 上探完判 ok。规避见 design.md §4"已知漏报边界";加锁即可覆盖
  (mutex 本身是调度点)。
- **非确定性来源**(模型需规避):GC/finalizer/`AddCleanup` 时序、`sync.Pool`、指针地址(ASLR/
  分配器/裸指针 hash)、cgo/外部进程——均不可控,模型不得依赖。
- **monkey-patching 测试库(mockey)在当前原型工具链上链接失败——根因是 Go 版本,不是 weave 机制**:
  `bytedance/mockey` 的 arm64 打桩汇编引用 `runtime.duffcopy`/`duffzero`,而本原型基于的 **go1.27-devel
  已移除 arm64 的 Duff's device**(提交 `e4291e484c runtime: remove duff support for arm64`,`src/runtime/`
  下已无 `duff_arm64.s`),故 arm64 上这两个符号不存在 → 链接期报 `relocation target runtime.duffcopy
  not defined`。**实测(2026-07-16,kun/darwin-arm64):该失败在不带 `-weave` 时同样发生;go1.25.1 上
  mockey 正常**,可见与 `-weave` / weave 机制无关,纯粹是 go1.27-arm64 移除了 duff 符号、mockey 尚未适配。
  影响面:同目录所有 `_test.go` 编进同一个测试二进制,只要有兄弟测试(哪怕间接)引入 mockey,该包在
  本工具链上就构建不了。**更完整的复盘(2026-07-16,对最新版 mockey `d48df59`)**:duff 那个坑新版
  已修(1.26+ 走 `linkname.FuncPCForName("runtime.duffcopy")` 运行期软查找、符号不存在则降级,取代旧版
  链接期硬引用);但最新 mockey **整体仍不支持 go1.27**——它硬依赖按 Go 版本写死的 runtime 内部
  (`doStopTheWorld`、`gGoroutineIDOffset`、`sysmonLockOffset`),这些文件的 build 约束**全部 `!go1.27`**
  (支持上限 = Go 1.26),在 go1.27 上这些符号一律 undefined。→ 结论:**mockey 支持上限 Go 1.26,而本
  原型是 go1.27-devel,纯版本错位、与 weave 机制无关**。规避:把 weave 用例放到不依赖 mockey 的包
  (如 kun `internal/sdk/mq/queue`);**根治:weave 基线选 ≤ Go 1.26**(不只 mockey,gohook 等一切硬依赖
  runtime 内部布局的库都会在 go1.27 碰壁),或等 mockey 出 go1.27 门控。`e4291e484c`(移除 arm64 duff)
  作者日期 2025-06-05、合入 2025-08-15。
  另需注意一个**语义**问题(与构建无关):mockey 在运行期改写目标函数的序言跳转到 mock,而 `-weave`
  的内存插桩在原函数体里——被 patch 掉的原函数体不会被 weave 探索(执行的是 mock);通常这正是你想
  mock 掉的外部依赖,可接受,但要清楚 weave 只探索"真正执行到的代码"。
- **把 weave 用例对准"共享状态原语"而非"扇出编排"**:worker-pool 式代码(`sync.WaitGroup` + 按批
  spawn N 个 goroutine)在 `-weave` 下会因"同时可运行的 goroutine × 每个 goroutine 的插桩内存操作"
  组合爆炸(kun `queue.Worker.Run` 实测撑爆 100 万预算)。真正需要验证的并发安全其实只在少数
  mutex 保护的操作里(如 `AddErr` 的切片 append)。直接对这些操作起 2 个 goroutine 竞争(绕开
  Run 的编排)即可把状态空间压到几十条(26/16),同时精确覆盖竞争点——去掉锁后 weave 4 条调度即
  报 `lost an append`。

---

## 8. 分步验证方法(降低 runtime 改错风险)

1. **只加字段 + no-op 钩子**,跑 `go test runtime sync internal/synctest testing/synctest` 确认零回归。
2. **round-robin 串行化**,验证真实 `go func()` 严格一次跑一个。
3. **接真实 sync.Mutex**(internal/sync 入口加 schedPoint),验证真实 Mutex 用例可探索。
4. **接 chan/select**,移植 L3 引擎,`synctest.Test`(`-weave`)可用。
5. **失败 seed 重放**;差分健全性对拍(`TestDPORSoundnessSuite`)。

**日常构建/测试**:
```sh
cd src
GOTOOLCHAIN=local ../bin/go test -count=1 internal/weave/ testing/synctest/  # 核心引擎(无 -weave)
GOTOOLCHAIN=local ../bin/go test -count=1 -weave internal/weave/ testing/synctest/  # 含内存插桩
GOTOOLCHAIN=local ../bin/go test -count=1 runtime sync sync/atomic internal/synctest testing  # 回归
GOTOOLCHAIN=local ../bin/go test -count=1 -run TestIntendedInlining cmd/compile/internal/test # 内联预算
cd ../weavedemo && ./check.sh                                  # 演示用例的行为断言
```
工具链按需重编改动的 std 包,纯 Go 编辑无需 `make.bash`。

**操作坑**:
- **改了 `cmd/compile`(如 D19)必须重装编译器**:`cd src && GOTOOLCHAIN=local ../bin/go install cmd/compile`,
  否则 `go test` 用的还是旧工具链、改动不生效。只改 runtime/std 包时 `go test` 会自动重编,不用重装。
- **环境变量不进 `go test` 缓存** → 用 `WEAVE_*` 做 A/B 对比时必须加 `-count=1`。
- 本机 shell 的 `cd` 被 zoxide 接管且会打印噪声:命令前加 `export _ZO_DOCTOR=0`,或用绝对路径。
- **gopls 的诊断多是工作区噪声**("undefined: runSchedule"/"use of internal package"/"not in workspace"
  ——因为 `src/` 不是它的 workspace module),以实际 `go build`/`go test` 结果为准。

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
  仅在失败 trace 含内存 transition 时带 `-weave`。逻辑现居 `testing/synctest/weave.go` 的 `replayCommand`。
- **wall-clock timeout 明确为 runs 之间的 soft limit**(单条卡死 schedule 需外部 `-timeout`;
  抢占看门狗仍是已知缺口,见 §5.A.2)。

**Go 仓库集成门禁**
- `testing/synctest` 在 `go/build/deps_test.go` 里获得 `internal/weave` + `path/filepath` 依赖;
  `testing/weave` 条目已移除。`go/build.TestDependencies` 通过。
