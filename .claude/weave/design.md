# weave 设计文档

> 本文说明 weave 的**设计取舍与理由**:为什么这样分层、为什么复用 synctest、DPOR 如何保证健全、
> 范围边界在哪、以及一份 ADR 决策记录。整体结构见 [arch.md](arch.md);实现映射与状态见
> [imp.md](imp.md)。
>
> 命名 `weave`(编织):goroutine 即线,探索交错 = 把多根线编织起来看所有织法。与 `synctest`
> 成对——`synctest` 负责隔离 + 假时钟,`weave` 负责交错枚举。本文中 "loom" 专指作为参考的
> Rust 原版工具。

---

## 1. 问题与目标

系统性遍历多 goroutine 的交错,确定性复现"只在特定调度下出现"的并发 bug。相对 loom 的两点
改进(已与用户确认):

1. **Go-native / 零改写**:loom 强制把 `std::sync` 换成 `loom::sync`(靠 `cfg(loom)` 条件编译),
   这是它最大的工程学负担。weave 让**未修改的、用真实 `chan`/`sync.Mutex`/`sync/atomic` 的
   代码**直接被探索——靠深度复用运行时 `synctest` + 编译器插桩(类比 `-race`)。
2. **探索引擎是 DPOR**(动态偏序规约),而非停留在随机/穷举。

---

## 2. 设计原则

### 2.1 只在同步操作处切换(不逐指令)

同 loom / CHESS:调度点 = 同步操作(chan/mutex/atomic/goroutine 起止 + 显式 Yield),不在任意
指令处切换。逐指令交错的状态空间不可行;而**非原子内存竞态本就是 `-race` 的职责**。需要更细
粒度时用 L1 内存插桩档(`-weave`),按内存访问切换。→ 见 ADR D5。

这也决定了 weave 与 race 的分工:weave 查"某个合法交错下程序对不对",race 查"有没有未同步的
并发访问"。二者互补(§6)。

### 2.2 复用 synctest,而非另造隔离机制

Go 已有的 `synctestBubble` 恰好提供了 weave 所需的一半能力:goroutine 分组(创建时无条件继承
`newg.bubble = callergp.bubble`)、全组 durably-blocked 静止检测(`changegstatus`)、假时钟、
对象归属。weave 只需补另一半:**串行化 + 调度顺序控制 + 跨 run 系统枚举**。因此 `controlled`
字段直接加在 `synctestBubble` 上——weave 是 bubble 的一种"模式"。→ 见 ADR D2。

关键洞察:loom/CHESS 的"同步操作处切换"在 Go 里恰好对应 `gopark`/`goready` 边界 + chan/sema/
select 的 bubble 路径 + goroutine 起止。**拦截这些既有 choke point 即可,无需重写 M:N 调度器。**

### 2.3 零改写靠 runtime + 编译器

- chan/mutex/select 本就是调用进 runtime 的函数 → **runtime 钩子(L2)**直接拦真实类型。
- atomic/裸内存是编译器 intrinsic / 无调用点的 load-store → **编译器插桩(L1)**,类比 `-race`。

bubble 归属在创建时继承,跨 package 自动生效,对被测代码透明。代价是必须动 runtime 与 compiler
(用户已明确接受)。→ 见 ADR D3。

---

## 3. 各层设计

### 3.1 L2:控制调度器

在 `synctestBubble` 上加 `controlled bool` + `weaveCtl *weaveControl`。开启后:

1. **run-token 串行化**:每个受控 bubble 恰有一个运行令牌,参与者只有持令牌才 `_Grunning`。
   到调度点释放令牌、记录 transition、park,等控制器再次授予。借用 `gopark`/`goready` 作暂停/
   恢复,**不改 `findRunnable`/`schedule` 主逻辑**。
2. **令牌在三个中心点交接**(免逐原语改代码):
   - `ready`:受控下被同步操作唤醒的参与者不入 OS runqueue,而是加入控制器 runnable 集合
     (`weaveEnqueue`),保持串行。
   - `park_m`:参与者真实阻塞(chan/mutex/…)时把令牌交给下一个 runnable 参与者
     (`weaveOnBlock`);无人可跑 = 死锁。
   - `goexit` / `Yield`:退出或显式让出时同样交接。
3. **调度点** = 参与者创建(`newproc`)、退出、每次同步操作前、内存访问(L1)、`weave.Yield`。
   全组 durably-blocked 或全部退出 = 一条 run 的结束/死锁点,由控制器自判并唤醒 root。
4. **静止判定复用 synctest 但记账独立**:受控 bubble 由控制器自管存活计数,`changegstatus`/
   `incActive`/`decActive` 对受控 bubble no-op(它们是为处理 park/wake 竞态的空闲计数而设,
   受控 bubble 用 run-token 不变量取而代之)。控制器"等令牌"的 park 用**不算 idle 的**
   `waitReasonWeaveScheduled`,避免被误判为 durably-blocked。

> 命名说明:早期规划里 L2↔L3 接口叫 `weave_choose`/`weave_event`;实际实现是等价但更少的原语
> `weaveRunSchedule`(跑一条调度并带回 trace)+ `weaveSchedPoint`/`weaveYield`/`weaveWait`。

### 3.2 L1:编译器插桩(共享内存)

`sync/atomic` 是编译器 intrinsic、普通内存 load/store 无调用点,靠 linkname 拦不住,须走编译器。
参照 race detector(`-race` → 编译器插入 `raceread`/`racewrite`):

- 新增构建模式 `-weave`(`go test -weave` / `-gcflags=-weave`),对命令行包插桩。每个共享内存
  读/写 → `runtime.weaveread`/`weavewrite`(及 range 版),成为调度点 → DPOR 在数据竞态处也能切换。
- **与 sanitizer 共享同一条插桩 pass**(instrumentation fusion):没有另起炉灶,而是复用
  `cmd/compile/internal/ssagen/ssa.go` 的 `instrument2`,只在末尾按模式发不同 hook。
  `base.Flag.Weave` → `Cfg.Instrumenting=true`,走同一门控;composite/scalar 分流与 race 对称。
- **白拿逃逸剪枝**:`instrument2` 统一调 `ssa.IsSanitizerSafeAddr`——栈地址/RODATA/闭包只读
  字段一律不插桩。不逃逸的 goroutine-local 变量落栈上被剪掉,不会变调度点。对 weave 这是 sound
  的(局部量不可能被别的 goroutine 观察,重排它天然 independent),还直接缩状态空间。
- **runtime 不被插桩**:`objabi.NoInstrument` 包在 `gc/main.go` 里把 Weave 关掉,避免
  `weaveread` 递归回自身。
- **与 sanitizer 互斥**:`-weave` 与 `-race`/`-msan`/`-asan` 共享同一 pass,同开会静默丢掉 weave
  插桩。双重保护:`cmd/go` 的 `weaveInit`(友好报错)+ `cmd/compile` flag 校验。
- **值显示**:读插桩传 size,运行时按类型读内存显示读到的值;写插桩把整型/bool 右值零扩展后传
  `weavewriteval` 显示写入的新值(排除 uintptr 等)。用于失败 trace 可读性。

**现状**:内存插桩已完成;`sync/atomic` **类型化 API** 已纳入(库级钩子,见 D18),自由函数与 `atomic.Value`
待做。故 design 早期"默认只插 atomic、内存作可选高强度档"的设想与现实相反——现实是内存先做、atomic 后补。

### 3.3 L3:DPOR 探索引擎

`internal/weave`,纯 Go,跑在 driver goroutine,经 linkname 被 L2 回调。

- **transition 与 happens-before**:每 run 记录 transition 序列(wid、op、addr、enabled 掩码、
  PC、值)。happens-before 用**两级保守近似**:
  - **程序序**:同一参与者的先后 transition 天然 HB。
  - **channel happens-before**(`channelHB`):第 k 次 send HB 第 k 次 recv(channel FIFO);
    close HB 其后 recv。用 per-participant 向量钟实现。
  - **只建模 channel 边,其余同步(mutex/rwmutex/cond/waitgroup)一律视为无序**。这是**有意的**:
    单条 acquire/release 链无法表达读者并发,强行加序可能引入**假 HB 边**而漏掉真实反转。
- **conflict 判定**:两个不同参与者的 transition 依赖 ⟺ 触及重叠内存/同一同步对象、且至少一写。
  - **内存读写按字节区间重叠判定**(`[addr, addr+size)`),而非裸地址相等:复合对象(结构体/数组)
    的整体写走 range 钩子并记录**完整 width**,故能与任一子字段的读判为冲突(否则整struct写 vs
    字段读会被误判独立而漏状态)。读/读重叠:independent;任一写重叠:conflict。
  - **同步对象**(chan/mutex/…)按对象身份(同地址)判冲突。
  - **select 特判**:select 事件在 trace 里以 `addr=0` 记录(它检查的 channel 集合表达不了),
    故保守地视 select 与任意 channel 操作及另一个 select 冲突(忽略地址)。
  - **非阻塞 channel 操作**(select+default 编成 `selectnbrecv`/`selectnbsend`)用独立 op 码
    `weaveOpChanSendNB`/`RecvNB`:参与 conflict(可被重排),但 `channelHB` 忽略它们(不建立 HB)。
- **source-DPOR 主循环**:跑一条 schedule 到结束/死锁 → 回溯扫描,对每对**冲突且并发**的
  transition,在其竞争前驱的调度点 backtrack 集合里加入被延迟的 wid → 从 backtrack 驱动下一条
  schedule,只探索每个偏序等价类的一个代表,指数级剪枝。odometer 穷举保留在
  `exploreExhaustive` 里做等价性对拍。
- **select-case 枚举**:多就绪 case 是"选谁跑"之上的第二个选择维度,穷举枚举(`selD`/`selCase`),
  与 wid 级 DPOR 组合。
- **上界**:调度数预算(`DefaultMaxSchedules`/`WEAVE_MAX_SCHEDULES`,超限标 `Truncated`)+
  wall-clock 超时 + **抢占计数上界**(CHESS 式 context bounding,`ExploreBounded`):限制 ≤c 次
  抢占的调度,bound 内完备且大幅剪枝。

### 3.4 L4:公共 API —— 就是标准库 `testing/synctest`(无 weave 专属 API)

没有 weave 专属 API(见 D12)。用户写普通 `testing/synctest` 用例:

```go
func Test(t *testing.T, f func(*testing.T))   // 标准库签名
func Wait()                                   // 阻塞到其余 goroutine durably blocked / 退出
```

`go test`(不带 flag)= 一次普通 synctest 单跑;`go test -weave` 下 `synctest.Test` 改派到 L3 探索引擎:
`testing/synctest/weave.go`(`//go:build weave`)的 `weaveExplore` 起 controlled bubble、驱动引擎每条
schedule(每条经 `testing.testingWeaveTest` 把 f **内联作为 bubble 的主参与者**跑——而不是另起子
goroutine,否则真 root 会成为 `weaveWait` 的幽灵等待者;失败转 panic 供引擎捕获);
任一调度 panic/死锁 → `t.Errorf` + 逐步交错 + goroutine 图例 + seed;截断 → `INCOMPLETE`;成功 → `t.Logf`。
`synctest.Wait()` 在受控 bubble 内自动路由到 weave 屏障(`runtime.synctestWait` 判 `bubble.controlled`)。

---

## 4. 健全性(soundness)

weave 的健全性目标:**不漏报**——凡是存在于合法交错空间里的 bug,探索都应能到达。核心手法是
**保守近似**:

- **缺 HB 边只会多探,不会漏探**:少一条 happens-before 边 → DPOR 认为更多 transition 对可反转
  → 探索更多调度 → 健全。**危险的是"假 HB 边"**(spurious edge)——它会让 DPOR 误以为两个冲突
  操作已定序而跳过必要反转 → 不健全。因此所有近似都朝"宁可少建边"的方向:
  - mutex/rwmutex 排除出 channelHB(避免假读者序)。
  - select 以 addr=0 记录,用**忽略地址**的 conflict 换取"绝不漏 select 与并发 chan 的反转"。
  - **channelHB 对被"消费但未出队"的 send clock 保守处理**:成功的非阻塞 recv / select-recv 会
    收走一个值但 FIFO 模型不 pop 发送队列,若放任,后续普通 recv 会 pop 到过期 send clock → 假边。
    故 **NB 触及的 channel 一律 taint、含 select 的 run 直接禁用 channel HB**(其消费的 channel 无从
    辨识),这些 channel 只保留程序序。少边=多探=健全。
- **内存冲突用区间重叠 + 记录 width**:避免整struct写与字段读被误判独立(见 §3.3)。
- **抢占上界约束的是"实际执行的 schedule"**:bounded 模式下默认后缀 sticky(仍可运行就继续当前
  参与者),使非强制后缀零抢占;加上只添加 ≤c 的 backtrack,执行出的每条 schedule 都 ≤c 抢占,故
  "≤c 抢占内无 bug"是真保证,不会误报越界后缀产生的 failure。
- **程序序保守近似**替代逐对象向量钟:保守但健全。
- **逃逸剪枝是 sound 的**:被剪掉的栈局部变量不可能被别的 goroutine 观察,重排它天然 independent。

**插桩粒度:runtime 调用内部无调度点(与 `-race` 互补,无净盲区)**:调度点只在**被插桩的共享内存
访问**(标量/结构体字段的 load/store,复用 race 的 `instrument2`)、channel 与 sync 原语处产生。
**经由 runtime 调用完成的访问不产生调度点**——最典型的是 **Go map**(`mapaccess`/`mapdelete` 是
runtime 函数,runtime 按 `NoInstrument` 不插桩,用户侧调用点也不降级成 weaveread/weavewrite)。乍看
像"漏报边界",但**深究下来没有净盲区**,因为要出现「两个 map 操作之间需要交错才能暴露的 bug」,只有
两种情形:

1. **两个 map 操作之间无任何同步** → 本身就是**数据竞态**。weave 因串行化确实探不到(见下),但
   **`-race` 稳抓**、普通跑还会触发 runtime 的 `concurrent map writes` panic(尽力而为)。这正是 weave
   有意留给 `-race` 的一类(见 §6)。
2. **两个 map 操作各自被 mutex/channel 保护、但 check-then-act 跨了同步边界**(原子性违背,**非**竞态)
   → `-race` 看不见,但**交错点落在 `Lock/Unlock` 等已插桩的同步原语上,weave 照样枚举得到**——map
   内部是否透明根本不影响。

对拍证实(2026-07-17,scratch):无锁并发写同一 map,weave `PASS`(1379 调度,**不 panic**)而 `-race`
报 `DATA RACE`;加锁 map + 跨锁 check-then-act,`-race` 无报而 weave 32 调度即抓出 double-consume。
故两类恰好互补。

**须澄清的一点**:runtime 的 "concurrent map" panic 靠"两 goroutine 物理同时在 map 内部"触发,而
**weave 串行化(持 token 者一次跑完 map 操作,`hashWriting` 在同一次运行里置位又清位)使该 panic 在
weave 下永不触发**。所以不要指望 weave 自己 panic 出 map 竞态——那类交给 `-race`(稳)或普通跑(尽力)。
标量/字段上的无锁 check-then-act 仍由 weave 直接抓(对拍:标量 8 调度报出),因为标量访问是插桩点。

**验证方式**:差分健全性对拍——`TestDPORSoundnessSuite` 对每个模型断言 "DPOR 探索到的终态集合
== 穷举终态集合"。针对性回归:`TestSelectVsConcurrentSend`/`TestNonBlockingSelectVsSend`(select
与并发 send 的反转)、`TestChannelHBNoStaleEdge`(NB 消费不产生假边)、`TestStructWholeWriteVsFieldRead`
(整struct写 vs 字段读)、`TestPreemptionBoundConstrainsExecuted`(c=0 不报越界 failure)、
`TestSelectStaleSudogNoFalseDeadlock`(stale sudog 不误报死锁)。差分对拍历史上真的抓出过一个
GC 抢占恢复丢令牌的竞态(见 D10),以及一轮代码评审报出的一组健全性/正确性缺口(见 imp.md §9)。

---

## 5. 失败复现与报告可读性

- **seed**:失败时序完全由调度器在各决策点的选择序列(`choices []int` + select 选择)决定。
  编码成 seed 字符串 → `Replay(seed, f)` 或 `WEAVE_REPLAY=<seed>` 确定性重放同一交错。前提:模型
  除调度外确定(无 rand/真实时间/map 迭代序依赖),改代码则 seed 失效。→ 见 ADR D7。
- **goroutine 图例**:报告以稳定 id `gN`(`weaveWid`,按入组序分配)标识参与者。失败 trace 前
  打印图例,把每个 `gN` 映射到其 `go` 语句创建位置(源码 file:line + 外层函数):
  ```
    goroutines:
      g0: model root
      g1: supported.TestMutexABBADeadlock.func1 (mutex_abba_deadlock_test.go:22)
      g2: supported.TestMutexABBADeadlock.func1 (mutex_abba_deadlock_test.go:28)
  ```
  实现:`newproc` 里 `sys.GetCallerPC()` → `newg.gopc`;`weaveAssignWid` 按 wid 记进
  `weaveControl.spawnPC`,经 `weaveRunSchedule` 带回;`buildGoroutines` 用 `CallersFrames` 符号化。
- **源码行号 + 值**:内存 transition 记 caller PC → 符号化为 file:line;读/写显示值(§3.2)。

---

## 6. 与 `-race` 的关系(互补,非竞争)

- **找的 bug 类别不同(最本质)**:race 找数据竞态(对同址的无同步并发访问);weave 找**调度相关
  的逻辑 bug**——死锁、丢更新、不变量破坏、goroutine 泄漏、原子性违背。一段**完全 race-free**
  (同步都加对了)的代码仍可能丢更新/死锁,race 永远报不出、weave 能。反之纯非原子内存竞态
  weave 默认不逐指令枚举,交给 race。
- **被动观察 vs 主动控制**:race 被动旁观**这次恰好跑出的调度**,buggy 交错没触发就静默漏报;
  weave **接管调度**,串行化 + DPOR 系统枚举,不靠运气,且每条交错可 seed 复现。
- **happens-before 用途不同**:race 用 HB 判"这次访问算不算竞态";weave/DPOR 用 HB 判"两
  transition 是否独立"从而剪枝。二者都用类似 `-race` 的编译器插桩,但 race 只建 HB 图不改调度,
  weave 要在同步点改变调度。

一句话:race 回答"这一次跑有没有漏同步",weave 回答"在所有合法调度里有没有哪次会算错/死锁/
泄漏"。二者可叠加。

---

## 7. 范围与边界

weave 是**单元级、封闭的并发验证器**,不是全程序工具。控制范围 = synctest 泡泡 = "从测试闭包
派生的 goroutine"。这个边界的正确处理见 ADR D9:

- **A 类(状态跨 run 残留)**:同进程重跑 `f`,可变全局会带脏状态进下一遍。可用基于 `weavewrite`
  钩子的 undo-log 自动回滚缓解(roadmap 候选)。
- **B 类(泡泡外并发)**:init 期单例、全局池、真 I/O、真时钟、cgo 线程生在泡泡外,对 weave 不
  可见。这堵墙**非 weave 独有**(loom/Shuttle/Coyote/CHESS 全撞同一堵),是"系统化交错探索"方法
  的内禀边界。**不追求控制整个进程,也不改成 rr 类重放系统**;正确姿势是用**依赖注入 / fake**
  把不可控依赖(单例/时钟/I/O)在闭包内换成内存接缝。DI/seam 不是丑陋补丁,是此类工具的公认用法。

**非确定性来源**见 imp.md 的总表(select 多就绪、RNG/map 序、真实时间、I/O、GC/finalizer 等)。

---

## 8. ADR 决策记录

轻量 ADR:记录重要决策及理由,便于迭代时理解"为什么这样"而不重复讨论。

### D1 — 命名为 `weave`
短、小写、动词,契合 Go 命名习惯(`sort`/`race`/`sync`/`synctest`);隐喻精准(线=goroutine,
编织=探索交错);与 `synctest` 天然成对;标准库/flag 无占用。备选:`braid`、`interleave`。

### D2 — 深度复用 synctest bubble,而非另造隔离机制
bubble 已提供分组/静止检测/假时钟/对象归属;weave 只补调度控制 + 串行化 + 跨 run 枚举。
`synctest` = bubble(隔离+假时钟);`weave` = bubble + 调度控制层。二者分层依赖,非正交。

### D3 — 零改写靠 runtime + 编译器,而非 loom 式换类型
换类型是 loom 最大工程学负担。Go 里 chan/mutex/select 走 runtime → 钩子拦真实类型;atomic/裸
内存走编译器插桩。bubble 归属创建时继承,跨 package 自动生效。代价:必须动 runtime 与 compiler。
边界:仅"从 bubble 内派生"的 goroutine 被控制(见 D9)。

### D4 — 探索引擎目标 DPOR,原型先用穷举占位
最终用 DPOR;M0 原型先用穷举 DFS odometer 打通端到端,再替换。穷举完备易验证,能立刻证明引擎/
API/复现能力;`choose`/`event` 契约与 DPOR 一致,可平滑替换。穷举现保留在 `exploreExhaustive`
做等价性对拍。

### D5 — 只在同步操作处切换,不逐指令
同 loom/CHESS。逐指令交错状态空间不可行;非原子内存竞态是 race detector 的职责。需要更细粒度时
用 L1 内存插桩档按访问切换。含义:weave 查"某合法交错下程序对不对",race 查"有没有未同步并发
访问",互补。

### D6 — 原型里为何用 `NewValue` 而非普通 `int`(历史)
M0 原型(纯库级插桩)下调度器只能在"回调进调度器的操作"处切换,裸 `x = x + 1` 不可观测 → 丢更新
的坏交错生成不出来,故用 `weave.Value[T]` 把访问变成调度点。这是**原型层妥协的占位符**;M2
(runtime 钩子)+ L1(编译器插桩)落地后,普通 `int`/atomic 的访问自动变调度点,`NewValue`
从用户视角消失。原型库已删除,`weavedemo/` 现用真实类型。

### D7 — 失败复现用选择向量 seed
失败时序完全由调度器各决策点的选择决定;捕获该向量 → seed → `Replay`/`WEAVE_REPLAY` 百分百
重放。前提:模型除调度外确定;改被测代码 seed 失效。

### D8 — L2 的 runtime 钩子始终编译、纯运行时门控(不用 build tag)
**决策**:L2 的 **runtime** 钩子(chan/select/park/ready/newproc/goexit)**始终编译进去、无 build tag**,
`go test` 直接可跑 weave 测试。成本靠运行时门控(`weaveActive()` + `weaveGloballyActive` 单次全局 load),
普通程序只多一次可预测的 not-taken 分支,footprint 同 synctest。**为何不像 -race 用 build tag**:-race 在
每次内存访问插桩(遍布、无法廉价门控),必须编译期开关;L2 的 runtime 钩子只在 park/ready/newproc/goexit
少数 choke point,可廉价运行时门控。**L1 仍需 `-weave` 构建档**(内存插桩遍布每次访问)。

**范围已被 D20 收窄**:这条只对 runtime 钩子和 `internal/sync.Mutex` 成立。`sync` 的 RWMutex/Once/
Cond/WaitGroup 与 `sync/atomic` 的钩子后来改成了 build-tag 常量(`weaveEnabled`)门控——因为"运行期
门控"仍要付一次**函数调用**,而调用在内联成本模型里值 57 分,足以让上游正好卡在预算上的
`RWMutex.RLock`/`RUnlock`/`Once.Do` 在每个 Go 程序里失去内联。理由与代价见 D20。

### D9 — "全局状态 / 泡泡外并发"是品类边界
拆成两个可解性不同的子问题(详见 §7):
- **A(状态跨 run 残留)**:可用 `weavewrite` 钩子的 undo-log 自动回滚缓解。为何不用 `fork()` 拿
  干净快照:Go 多线程下 fork 不安全,这条路基本封死;undo-log 是更现实的等价物。
- **B(泡泡外并发)**:用户态不可根治,是方法内禀边界。**决策**:(1) 不控制整个进程、不改成 rr;
  (2) 最高优先级工程回应是把 B 从"静默出错"变"显式报错"`uncontrolled concurrency detected`
  (roadmap 候选,复用 synctest 跨泡泡检测);(3) 定位澄清:weave 是单元级 hermetic 验证器,
  全程序/真并行/真 I/O 归 `-race` 与集成测试。

### D10 — 受控 bubble 内在 `waitunlockf` 之前置 `weaveBlocked`(关闭 block-wake 竞态)
`ready` 只截获控制器真正阻塞过的参与者(`g.weaveBlocked`,park_m 阻塞路径设置),并排除
`waitReasonPreempted`(GC 抢占恢复不丢令牌)。**残余竞态**:`park_m` 调 `waitunlockf`(如释放
`c.lock`)会让该 g 对另一 M 上的 waker 可见;若此刻 `weaveBlocked` 尚未置位,竞态 waker 在
`ready` 读到 false → 走普通路径把参与者变 OS-runnable、绕过控制器 → 两个并发运行者 → 误报死锁。
**修复**:把 `weaveBlocked=true` 移到 `waitunlockf` **之前**(park 对 waker 可见之前),park 中止
分支回退。仅改 `proc.go`,非受控 bubble 零影响。实证:`TestSyncPrimitivesExplorable` 由 ~1/20
flaky → 60/60。是 GC-抢占家族的最后残余。

### D11 — 受控 bubble 内的假时钟推进(原禁用,现已支持)
**历史(已废弃)**:早期 `synctestRun1` 的 controlled 分支直接走 `weaveRootWait` 并 return,跳过
synctest 的假时钟推进循环 → `time.Sleep`/`time.After`/`NewTimer`/`context.WithTimeout` 注册的假
定时器永不触发 → 永久 park → **误报死锁**。当时决策是"先禁用",时间/IO 用内存接缝(channel/
select/`context.WithCancel`)建模。

**现状(已实现)**:`weaveRootWait` 现在**就是** weave 版的静止推进循环。当所有参与者 durably
blocked、无 runnable、但有 pending timer 时,控制器(root)把 `bubble.now` 推进到下一个到期时刻、
调 `bubble.timers.check` 触发到期定时器;定时器回调经 `ready`→`weaveEnqueue` 唤醒参与者,再由
run-token 交回令牌。只有"无 runnable 且无 pending timer"才是真死锁(`weaveOnBlock`/`weaveOnGoexit`
用 `bubble.timers.wakeTime()>0` 区分 advanceClock 与 deadlock)。于是 `time.Sleep`/`time.After`/
`NewTimer`/`context.WithTimeout` 在 `synctest.Test`(`-weave`)内**可正常工作**。定时器**触发顺序**沿用 synctest
的定时器堆(deadline)序(确定性,不新增 seed 维度);被唤醒参与者的**运行顺序**仍由 DPOR 在普通
调度点枚举。**小限制**:同一 deadline 的多个定时器的触发顺序不单独枚举;永不停止的 `time.Tick`
会无限推进时钟 → 探索受调度/时间预算约束(超限报 Truncated)。回归见 `internal/weave` 的
`TestFakeClock*`。

### D12 — weave 作为 `synctest.Test` 的 `-weave` 模式(免专属 API)

**落地状态(2026-07-17)**:`testing/weave` 包已删除,不存在 `weave.Test`/`weave.Wait`/`weave.Yield`
这类用户 API(`internal/weave.Yield`/`Wait` 只给引擎自测用)。用户只写标准 `testing/synctest.Test` +
`synctest.Wait`,`go test -weave` 即进入探索。三处关键实现:
(1) `testing/synctest/{weave.go,weave_off.go}` 按 `weave` build tag 分流 `weaveExplore`,并把富报告
(trace/图例/seed)迁入此处;(2) `testing.testingWeaveTest` 把 f **内联作为 bubble 的主参与者**跑(而非
`testingSynctestTest` 的子 goroutine——否则真 root 阻塞在 signal 上成为 `weaveWait` 的幽灵等待者→伪
死锁,且模型爆炸);(3) `runtime.synctestWait` 判 `bubble.controlled` 时路由到 `weaveWait`。deps 图
(`go/build/deps_test.go`)与 `-weave` flag help(`build.go`/`compile/doc.go`/`alldocs.go`)已同步。
weavedemo 全部改为 `synctest.Test`;`go test -weave` 下正确用例 PASS、bug 演示 FAIL,报告与旧版一致。
下方为原型阶段的动机与取舍记录。


**动机**:`weave.Test(t, func())` 与标准库 `synctest.Test(t, func(*testing.T))` 语义高度重合——都圈定
"泡泡 + 可重放闭包 + setup 在外"。为最大化易用性与采纳率(类比 `-race` 是模式而非 API),让**已经写好
的普通 `synctest.Test` 用例,只加 `go test -weave` 就跑进系统化交错探索,零改写、无 weave 专属依赖**。

**可行性(已逐项验证,2026-07-17)**:
- `-weave` 已注入全局 `weave` build tag(`cmd/go/internal/work/init.go`,仿 `-race` 的 `race`),故可用
  `//go:build weave` 文件在 `testing/synctest` 内分流。
- `testing.testingSynctestTest(t, f) bool` **本就是"每调一次建一个全新子 T、返回通过/失败"的可重放
  单轮原语**(`testing/testing.go`),正是 weave 每条 schedule 需要的。
- 无 import 环:`testing` 不依赖 `testing/synctest` 也不依赖 `internal/weave`,故
  `testing/synctest → internal/weave → testing` 无环。

**原型实现**:`testing/synctest/Test` 开头加 `if weaveExplore(t, f) { return }`;`weaveExplore` 有两份
构建分流——`weave_off.go`(`//go:build !weave`)返回 false(普通 `go test` 保持单跑);`weave.go`
(`//go:build weave`)把每条 schedule 的 body 设为 `if !testingSynctestTest(t,f) { panic(sentinel) }`
(失败转 panic 供 weave 捕获),调 `weave.ExploreBounded` 探索,结束后在泡泡外的真 `t` 上报告(含
`WEAVE_REPLAY` 种子);`WEAVE_REPLAY`/`WEAVE_MAX_SCHEDULES` 环境变量复用。

**验证**:同一个只用 `synctest.Test` 的丢更新用例,普通 `go test` PASS(单跑漏报),`go test -weave`
28 条 schedule 抓出 `x=1`;正确版 `-weave` 下 explored 3556 PASS(无误报);标准 `testing/synctest`
(不带 `-weave`)与 `internal/weave` 全回归通过。

**当时的两个遗留,后来都已解决**:(1) 失败经"子 T 失败 → 转 panic"桥接时,断言原文被 sentinel
覆盖 → 已由 **D17** 保真(报告直接显示 `t.Fatal` 的原文);(2) 走 `testingSynctestTest` 的子 T +
tRunner 会引入额外调度点、放大模型 → 已改为 `testingWeaveTest` 把 f **内联作为 bubble 的主参与者**
跑,绕开 tRunner。**仍然成立的隐式要求**:模型必须跨 run 幂等(见 D9 A 类)。

### D13 — 默认自动迭代加深抢占上界(替代无界搜索)

**决策**:未设 `WEAVE_MAX_PREEMPTIONS` 时,不再无界搜索,而是按抢占 0,1,…,`defaultPreemptCeiling`(=2)
**迭代加深**(L4 `exploreIterative` 逐层调 `ExploreBounded`),命中失败即停、否则报 "up to K preemptions"。
**理由**:实测真实代码(含 net.Pipe 协议握手)在无界下状态空间爆炸(一个请求/响应 demo 33s 撞满 100 万
预算 INCOMPLETE);而 CHESS 经验是绝大多数并发 bug 在 ≤2 次抢占出现。迭代加深让**反例用最少抢占**
(最易读),并把"零配置开箱"从"爆炸"变成"秒级 PASS 且给出 ≤K 抢占的保证"。设了 env 则以其为上限
(单次 `ExploreBounded` 已 complete-within-itself,不再迭代)。代价:PASS 用例白跑低层(可接受,低层小)。

### D14 — 失败报告可读性:对象标签 + 死锁等待对象

**决策**:轨迹里无源码位置的同步对象由裸地址 `@0x...` 改为稳定标签 `mutex#1`/`chan#2`/`cond#`/`wg#`/
`once#`/`mem#`(`addrLabeler`,按操作种类推断、同址复用编号);死锁报告在 "all goroutines blocked" 后
**逐 goroutine 列出等待对象**(`gN blocked on lock mutex#4`)。**理由**:裸指针无法看出"是不是同一个锁",
ABBA 死锁尤其难读。等待对象**直接从 trace 每个参与者的最后一步推断**(它 park 在那个调度点),故是纯
渲染层、不动 runtime。标签器在 trace 与死锁列表间共享,保证同址同名。

### D15 — 假时钟:基准对齐、假阴性提示、非并发测试跳过

**决策(三点)**:
1. **基准对齐(bug 修复)**:`weaveRunBubble` 曾漏设 `bubble.now`,使 `-weave` 下 `time.Now()` 返回 1970
   而非普通 synctest 的 2000。把 `synctestBaseTime` 提为 runtime 包级 const,两条路径共用。
2. **假阴性提示**:假时钟只在全体 durably blocked 时推进,故"超时 vs 一直可运行的事件"竞争会漏探
   (假阴性)。某调度里参与者全退出却仍有 pending timer 时,`weaveControl.unfiredTimer`→`Result.UnfiredTimer`,
   成功报告追加提示"把事件对齐到定时器边界"。**不自动修复**(对齐是模型侧的事),只诊断引导。
3. **非并发测试跳过**:`testing/synctest` 里验证 testing-package 交互输出(fork 子进程断言非-weave 格式)
   或重量级/压力(net/http 集成、100 定时器)的用例,在 `-weave` 下语义不适用/超容量,用 build-tag 常量
   `underWeave` 跳过(仿标准库对 `-race` 的 `//go:build !race`)。修后 `-weave` 下 `testing/synctest` 全绿。

### D16 — channelHB 对"channel 当锁"的 token-recycling taint(DPOR 健全性修复)

**问题**:`channelHB`(explore.go)用"第 k 个 send happens-before 第 k 个 recv"的 FIFO 配对做 DPOR 的
happens-before。对**当锁/信号量用的 channel**(同一 goroutine `<-ch` 获取、`ch<-` 释放,循环复用同一 token),
这个配对**不随交错稳定**:某个 recv 到底配对哪个 send 取决于调度。用它做 HB 会把两个竞争同一 channel 的
receiver **虚假地定序**,从而剪掉到达死锁的那个反转——`weavedemo` 里用 capacity-1 channel 当锁的 AB/BA
死锁因此被漏报(而同构的 `sync.Mutex` 版能抓到,因为 mutex 本就被 channelHB 排除、无 HB 边)。

**决策**:若某个 channel 上**存在一个 participant 既 send 又 recv**(token 复用 / 锁语义),则把该 channel
taint(与 NB op / select 同样处理),其 FIFO 序不再当作 HB。生产者/消费者 channel(send/recv 参与者不相交)
不受影响,HB 剪枝照旧。**健全性**:少一条 HB 边只会让 DPOR 多探反转、绝不漏探(与既有 taint 同理),故安全;
代价是锁式 channel 的搜索变宽(有抢占上界兜底)。

**验证**:`internal/weave` 全部差分健全性/等价用例(`TestChannelDPOREquivalence` 要求 DPOR 运行数 ≤ 穷举、
`TestDPORSoundnessSuite` 要求终态集合相等)仍通过;`weavedemo/supported/channel_lock_deadlock` 现能被抓到
(从 `unsupported/` 移入 `supported/`)。

### D17 — 失败报告显示测试断言原文(不再是通用 sentinel)

**问题**:模型用 `t.Error/Fatal`(而非 `panic`)失败时,`testingWeaveTest`(testing.go)用 `runtime.Goexit`
后在 defer 里 panic 一个通用 sentinel `weave: schedule failed`,weave 报告里失败原因就成了这句无信息的话
(而 `panic("msg")` 的用例能显示 `msg`)。丢了 `t.Fatal` 的真实信息(如 "lost update")。**根因**:synctest 子
`t`(`isSynctest`)的日志经 `destination()` 重定向到父 `t`,故子 `t.output` 为空,且 `-v` chatty 模式会直接
streaming 不缓冲——照搬 output 拿不到。

**决策**:在 `common.log` 里,当 `isErr && c.isSynctest` 时把该行(此时是未加 framing 的纯文本 `file:line: msg`)
另存到子 `t` 的 `weaveFailLog`;`testingWeaveTest` 失败分支改 panic 一个携带该文本的 `*weaveTestFailure`
(其导出方法 `WeaveTestFailure() string` 让 `testing/synctest` 能识别)。`formatOutcome` 对实现该接口的失败值
**直接显示原文、不加 "panic:" 前缀**(真正的 panic 仍显示 `panic: v`)。此改惠及所有基于 `t.Fatal/Error` 的用例。

**验证**:`testing`/`testing/synctest`/`internal/weave` 全绿;`weavedemo` 里 `TestLostUpdate` 等报告从
"panic: weave: schedule failed" 变为 "lost_update_test.go:22: lost update"。

### D18 — `sync/atomic` 类型化 API 作为调度点(库级钩子,非编译器)

**问题**:`sync/atomic` 操作既非调度点也不记录,故用 atomic 的无锁代码(如 `atomic.Load()+Store()`
组成的非原子 RMW)探索不到——曾列为"唯一大缺口"。早期设想以为"atomic 是编译器 intrinsic,须走编译器
插桩"。

**决策**:`sync/atomic` 的**类型化方法**(`Int64`/`Uint64`/`Int32`/`Uint32`/`Uintptr`/`Bool`/`Pointer[T]` 的
`Load/Store/Swap/Add/CompareAndSwap/And/Or`)其实是**普通 Go 方法**(体内调用 intrinsic 函数),可在 `type.go`
方法入口调 `weaveAtomic(op, &x.v)` 把每个操作变成调度点——完全类比 `sync.Mutex` 经 internal/sync 的库级钩子,
**无需改编译器**。op 分 Load/Store/RMW 三类;DPOR 里按对象地址冲突、两个 atomic load 视为独立、不建 HB(同
mutex)。**保内联**:钩子按 `weave` build tag 分流——`-weave` 下 `weave_on.go` 是真钩子,普通构建下
`weave_off.go` 是空 no-op,编译器内联掉,故普通程序 atomic 零开销、方法仍可内联(不碰 inl_test 的既有约束)。

**范围**:类型化 API 已覆盖(典型无锁代码都用它)。**未覆盖**:自由函数(`atomic.LoadInt64` 等,是编译器
intrinsic,须仿 `-race` 在 `-weave` 下关掉 intrinsic 再走带钩子实现)与 `atomic.Value`——留作后续。

**验证**:`sync/atomic`/`sync`/`internal/weave`/`testing/synctest` 全绿;`go build std` 通过;`TestIntendedInlining`
无新增回归(atomic 方法仍内联);`weavedemo/supported/atomic_lost_update` 现能被抓到(从 `unsupported/` 移入)。

### D19 — `-weave` 下多字段结构体赋值按字段存储(建模非原子写→撕裂读)

**问题**:多字节结构体赋值 `p = point{5,5}` 在语言层**不是原子的**(编译成多条 store,另一 goroutine 可读到
半更新的 `{5,0}`)。但 weave 之前把它记为**一个** `weavewriterange` 调度点 + **一条整体 store**,故读者插不进
两次字段写之间,撕裂读探不到(假阴性)。

**决策**:在 `cmd/compile` 的 `ssagen.storeType` 里,当 `base.Flag.Weave` 且目标是**无指针、多字段的结构体**
(`skip==0`)时,不发"整体 range 钩子 + 整体 store",而是**逐字段递归 `storeType`**:每个标量字段各得一个
`weavewrite` 钩子 + 一条字段 store。这样每次字段写都是独立调度点,读者可在字段写之间交错读到撕裂值。复用了
既有的 `storeTypeScalars` 字段分解(写屏障路径本就这么拆),正确性有保证。**门控**:仅 `base.Flag.Weave`;普通
构建与 `-race`/`-msan`/`-asan` 走原来的整体 store,**codegen 字节不变**。**范围**:限无指针结构体(避免写屏障复杂度);
含指针的结构体、slice/string 头、数组元素的撕裂留作后续。**注意**:改 `cmd/compile` 需重装编译器
(`go install cmd/compile`),`go test` 不会自动重建工具链。

**验证**:`internal/weave`(含 `TestStructWholeWriteVsFieldRead`)/`testing/synctest` 全绿;普通构建 `go build std`
+ `sync`/`strconv`/`encoding/json` 测试通过;`weavedemo/supported/struct_tearing` 现能被抓到(从 `unsupported/` 移入)。

### D20 — 库级钩子的成本边界:编译期门控 + 禁止内联泄漏

**问题**(审查发现,2026-07-29):`sync`/`internal/sync` 的钩子当年为兑现 D8"始终编译、运行期门控"而
**无条件**插在 `RWMutex.RLock/RUnlock`、`Once.Do` 等函数体里。代价被低估了:内联成本模型里**一次函数调用
就要 57 分**(预算 80),而 `RLock`/`RUnlock` 上游正好卡在 80。结果这三个函数在**用本 fork 编译的每一个 Go
程序**里都失去内联(`TestIntendedInlining` 三项失败,cost 150/151/143),直接违反 D8 自己的"非 weave 程序
几乎零开销"。另有一个更隐蔽的问题:`-weave` 的内存插桩只作用于**命令行包**,但**可内联的依赖函数体会被搬进
命令行包一起插桩**——`sync/atomic` 的类型化方法正是这种情况,于是 `weaveAtomic` 里那句
`if weaveGloballyActive != 0` 的**门控读自己变成了调度点**,每个 atomic 操作凭空多出两个 `read` transition,
atomic 密集模型的状态空间近乎翻倍(实测 `atomic_lost_update` 的 trace 里 6 步噪声)。

**决策**(三条):

1. **钩子体一律 `//go:noinline`**(`sync/weave_on.go`、`internal/sync/weave.go`、`sync/atomic/weave_on.go`)。
   函数体留在自己那个**未被插桩**的包里,门控读因此不再是调度点。调用方仍可自由内联(内联一个含调用的函数
   是允许的)。
2. **调用点用编译期常量门控**:`if weaveEnabled { weaveSchedPoint(…) }`,`weaveEnabled` 由 `weave_on.go`/
   `weave_off.go` 按 build tag 给出——与 `internal/race.Enabled` 完全同一手法。`if false` 连同函数体在
   **内联定价之前**就被删掉,所以普通构建的成本精确为零(不是"接近零")。
3. **唯一例外:`internal/sync.Mutex`** 的钩子保持"始终编译 + 运行期 `weaveGloballyActive` 门控"(D8 原样),
   因为 `Lock/TryLock/Unlock` 带着钩子仍在内联预算内。这保住了一条真实依赖:`internal/weave` 的引擎测试
   (`TestMutexDPORFindsDeadlock`/`TestTryLockExplored`)在**不带 `-weave`** 时也能探索 mutex 交错。

**代价与范围**:`RWMutex`(含 `Lock`/`TryLock`)、`Once`、`Cond`、`WaitGroup` 以及 `sync/atomic` 现在**只在
`-weave` 下是调度点**。用户侧无损——没有 `-weave` 根本进不了探索器(`weaveExplore` 本身就是 `//go:build weave`),
所以"不带 `-weave` 也有钩子"只服务于引擎自测。受影响的自测(`TestSyncPrimitivesExplorable`/`TestCondBroadcast`)
仍然通过,只是不带 `-weave` 时覆盖变浅;完整覆盖走 `-weave` 那一遍。

**顺带**:`RWMutex` 的读锁改用新 op `weaveOpRLock`/`weaveOpRUnlock`(trace 里显示 `rlock`/`runlock`、标签
`rwmutex#N`)。动机有两条:`rwmutex_writer_starvation` 这个 demo 的全部要点就是"RLock 排在 writer 后面",
而报告里 Lock/RLock 都印成 `lock` 根本读不出来;以及**复现命令**需要判断"这条 trace 是否只在 `-weave` 下存在"
(见 D21),而 `lock` 同时来自始终编译的 Mutex 和仅 `-weave` 的 RWMutex,不拆就无法判断。两者暂时**冲突关系
不变**(与 `opLock` 同等);把 RLock↔RLock 判为独立是一个健全的后续约简。

**验证**:`TestIntendedInlining` 由 3 项失败转全绿;`go build std` 通过;`runtime`/`sync`/`sync/atomic`/
`internal/synctest`/`testing` 全绿;`internal/weave` + `testing/synctest` 带与不带 `-weave` 全绿;
`weavedemo` 23 个预期失败 + `unsupported` 全 PASS;`atomic_lost_update` 的 trace 中 6 步门控读噪声消失。

### D21 — 探索模式下 `t.Skip` 是跳过,不是失败

**问题**(审查发现,2026-07-29):`t.Skip` 与 `t.Fatal` 一样经 `runtime.Goexit` 离开,而 `testingWeaveTest`
的 defer 只区分"panic"和"没跑完或 failed",于是**跳过被报成了失败交错**(`weave: found failing interleaving
… test failed`)。这个 bug 一直被 `runTest` 的 `underWeave` skip 掩盖(那些用例本来就不在 `-weave` 下跑)。

**决策**:defer 里先判 `t2.skipped && !t2.failed`,panic 一个 `weaveTestSkip` 哨兵;`weaveExplore` 识别它
(`WeaveTestSkip() bool` 接口)后调 `t.SkipNow()` 并停止探索。跳过原因不必随哨兵传递——`common.log` 早就把它
写进父 T 的输出了。同一判断也加在 `WEAVE_REPLAY` 分支上。

**顺带修正复现命令**:`traceNeedsWeave` 原本只认 `read`/`write`,而"只在 `-weave` 构建里存在的 transition"
现在还包括 atomic 与(D20 之后的)`rlock/runlock/once/cond */wg *`。漏判会打印一条**不带 `-weave` 的复现命令**,
而 `weaveChoose` 对"计划 wid 不可运行"是静默回退,于是重放会静默失配成"replayed seed, no failure"。已按新
清单补齐。

**验证**:`GO_WANT_HELPER_PROCESS=1 go test -weave -run '^TestSkip$' testing/synctest` 由 FAIL 转为 SKIP+ok
(`TestVerboseSkip` 同);`testing`/`testing/synctest` 全绿。

---

## 9. 待定 / 开放问题

### 已评估并否决:自动包装任意测试(免去 `synctest.Test` 这层包装)

**设想**(2026-07-29 评估):D12 已经免掉了 weave 专属 API,再往前一步是连 `synctest.Test(t, f)` 都不写——
让 `go test -weave` 直接接管普通 `func TestX(t *testing.T)`。**结论:不做。**

管道侧其实不难(约 2~2.5 天):钩子点是 `testing.tRunner`,现成的 `testingWeaveTest` 已经在做"建子 T、
把 f 当 bubble main 内联跑、失败转 panic、在 bubble 外的父 T 上报告";唯一的硬性约束是 driver 必须从
`testing/synctest` 下移到 `testing`(被测包若不 import `testing/synctest`,该包不进测试二进制,linkname 会
链接失败),选择器用 `WEAVE_TESTS=<regexp>` 则 cmd/go 零改动。成本也比传闻低:因为依赖包不插桩,一个
"json 编解码 + 两个 goroutine 争 map"的测试只要 118 条 schedule/0.01s,无并发的测试只要 1 条。

**否决理由是语义,不是工程量。** 实测(把各类真实测试代码放进 bubble)四类卡点:
`t.Run` panic、`t.Parallel` panic、真实 HTTP 因假时钟瞬间 dial 超时而失败、包级状态跨 schedule 泄漏导致
算错。普查你的真实仓库:约 1/3 测试文件用 `t.Run`、haven 有 21/59 用 `t.Parallel`、约 60% 碰 net/http,
而真正"有 goroutine 值得探索"的也只有 1/3。**默认自动包装等于把调试工具变成地雷。**

**顺带回答了"bubble 是否必须"**:必须。把 bubble 拆成四件事——成员身份+继承(可替代,`weaveCtl` 挂到 g 上
即可)、**假时钟**(替代不了,自己写就是重写 synctest,且没它就没有确定性与 seed 重放)、**封闭世界边界**
(替代不了,`c.bubble`/`isFake` 是"任一时刻只有一个 goroutine 可运行"这条不变量的**执法机制**;去掉它,
外部 goroutine 就能在令牌持有者运行时 ready 参与者,健全性静默丢失——这正是 D9 边界的物理基础)、
synctest 的策略性禁令。关键反直觉点:**去掉 bubble 一个卡点都解决不了**——`t.Parallel` 阻塞在整个测试
二进制共享的 `testState.startParallel` 上、`t.Run` 的 `tRunner` defer 要碰共享的 `tstate`/全局 `running`/
父链输出锁,这两条是跟**受控调度本身**冲突而非跟 bubble 冲突;真实网络失败源于假时钟;包级状态泄漏与
bubble 无关。

**若将来重启**:两个不需要动 bubble 的放松点——(1) `t.Run` 靠 leaf-wrap **绕过**(谁调用 `t.Run` 就不包它,
只包它的子测试);(2) 想要"真实 I/O + 受控调度"就加个逃生舱 `bubble.realTime`(保留 bubble、时钟走真实时间,
只探索纯内存交错,放弃时间维度的确定性,约 0.5 天,不建议默认开)。

- **把"推进假时钟"建模成一等的可枚举调度选项**(当前最值得做的能力缺口)。今天时钟只在全体 durably
  blocked 时推进,所以"超时 vs 一直可运行的事件"这类竞争探不到(假阴性,见
  `weavedemo/unsupported/timer_vs_event_false_negative`)。路径:把"推进到最近 deadline 并触发"做成
  伪参与者/额外维度,复用现有 select-case 枚举与 DPOR 回溯。触及调度核心(`weaveRootWait` 的
  advanceClock 逻辑)、搜索空间增大(需抢占上界兜底);好消息是**每条 schedule 都在全新 bubble 里跑
  (`now` 重置为 `synctestBaseTime`、timers 全新),所以时钟状态不需要回溯/重放**。
- 泡泡外并发的**显式检测报错** `uncontrolled concurrency detected`:如何在 synctest 跨泡泡检测
  基础上覆盖"共享地址被泡泡内外同时触碰"。
- 基于 `weavewrite` 钩子的 **undo-log 自动重置**:回滚粒度、只覆盖插桩内存的边界、开销。
- 弱内存(atomic C11 重排 / read-from 枚举)与 DPOR 的组合,复杂度可控性待验证。
- 状态空间预算/超时的默认策略与用户可调项;是否提供并行探索(多 worker 跑不同子树)。
