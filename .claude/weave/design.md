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

**现状**:内存插桩已完成;`sync/atomic` 尚未纳入(唯一大缺口,见 imp.md)。故 design 早期"默认
只插 atomic、内存作可选高强度档"的设想与现实相反——现实是内存已做、atomic 待做。

### 3.3 L3:DPOR 探索引擎

`internal/weave`,纯 Go,跑在 driver goroutine,经 linkname 被 L2 回调。

- **transition 与 happens-before**:每 run 记录 transition 序列(wid、op、addr、enabled 掩码、
  PC、值)。happens-before 用**两级保守近似**:
  - **程序序**:同一参与者的先后 transition 天然 HB。
  - **channel happens-before**(`channelHB`):第 k 次 send HB 第 k 次 recv(channel FIFO);
    close HB 其后 recv。用 per-participant 向量钟实现。
  - **只建模 channel 边,其余同步(mutex/rwmutex/cond/waitgroup)一律视为无序**。这是**有意的**:
    单条 acquire/release 链无法表达读者并发,强行加序可能引入**假 HB 边**而漏掉真实反转。
- **conflict 判定**:两个不同参与者的 transition 依赖 ⟺ 同地址、至少一写、或涉及同步对象。
  - 读/读同址:independent。任一写、或同址的同步操作:conflict。
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

### 3.4 L4:公共 API

`testing/weave`,镜像 `testing/synctest`:

```go
func Test(t *testing.T, f func())   // 受控 bubble 内反复运行 f,系统枚举交错
func Yield()                        // 显式调度点
func Wait()                         // 阻塞到其余参与者全部退出(替代 join channel)
```

`Test` 起 controlled bubble → 驱动引擎每条 schedule;任一调度 panic/死锁 → `t.Errorf` + 打印
逐步交错 + goroutine 图例 + seed;截断 → 报 `INCOMPLETE`(不静默当 ok);成功 → `t.Logf`。

---

## 4. 健全性(soundness)

weave 的健全性目标:**不漏报**——凡是存在于合法交错空间里的 bug,探索都应能到达。核心手法是
**保守近似**:

- **缺 HB 边只会多探,不会漏探**:少一条 happens-before 边 → DPOR 认为更多 transition 对可反转
  → 探索更多调度 → 健全。**危险的是"假 HB 边"**(spurious edge)——它会让 DPOR 误以为两个冲突
  操作已定序而跳过必要反转 → 不健全。因此所有近似都朝"宁可少建边"的方向:
  - mutex/rwmutex 排除出 channelHB(避免假读者序)。
  - select 以 addr=0 记录,用**忽略地址**的 conflict 换取"绝不漏 select 与并发 chan 的反转"。
  - 非阻塞 channel 操作**不建立 HB**:落空的 poll 绝不与某个 send 匹配;成功的 poll 少一条 HB
    边 = 保守多探。
- **程序序保守近似**替代逐对象向量钟:保守但健全。
- **逃逸剪枝是 sound 的**:被剪掉的栈局部变量不可能被别的 goroutine 观察,重排它天然 independent。

**验证方式**:差分健全性对拍——`TestDPORSoundnessSuite` 对每个模型断言 "DPOR 探索到的终态集合
== 穷举终态集合";select/非阻塞的反转各有专门回归(`TestSelectVsConcurrentSend`/
`TestNonBlockingSelectVsSend`)。差分对拍历史上真的抓出过一个 GC 抢占恢复丢令牌的竞态(见 D10)。

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
      g1: weavedemo.TestDeadlock.func1 (weave_test.go:79)
      g2: weavedemo.TestDeadlock.func1 (weave_test.go:85)
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

### D8 — L2 始终编译、纯运行时门控(不用 build tag)
**最终决策(已修订)**:L2 的 runtime 钩子**始终编译进去、无 build tag**,`go test` 直接可跑
weave 测试。成本靠运行时门控(`weaveActive()` + `weaveGloballyActive` 单次全局 load),普通程序
只多一次可预测的 not-taken 分支,footprint 同 synctest。**为何不像 -race 用 build tag**:-race 在
每次内存访问插桩(遍布、无法廉价门控),必须编译期开关;L2 钩子只在 park/ready/newproc/goexit
少数 choke point,可廉价运行时门控。**曾经的中间态**:一度做过 `weaveenabled` const +
`weave_on.go`/`weave_off.go` 的 build-tag DCE 方案(像 -race),后按用户要求去掉 tag,合并为单个
始终编译的 `runtime/weave.go`。**L1 仍需 `-weave` 构建档**(内存插桩遍布每次访问)。

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

### D11 — 受控 bubble 内禁用假时钟推进
`synctestRun1` 在 controlled 分支直接走 `weaveRootWait` 并 return,**跳过 synctest 的假时钟推进
循环**;且 `incActive`/`decActive` 对受控 bubble 早退。**后果(已实证)**:`weave.Test` 内
`time.Sleep`/`time.After`/`NewTimer`/`context.WithTimeout` 会注册假定时器但 `bubble.now` 永不
推进 → 定时器不触发 → 永久 park → **误报死锁**。**决策**:暂不融合假时钟推进(与调度枚举交互
复杂),保持"先禁用"。这是**明确边界**而非缺陷,与 loom/Coyote/synctest 同源——时间/IO 用**可被
weave 探索的内存接缝**建模:channel、select、sync 原语、`net.Pipe`、`context.WithCancel`
(channel/mutex 实现,可用;`context.WithTimeout` 走定时器,不可用)。实证见
`weavedemo/asyncio_test.go`。**未来**:可在控制器里集成——当所有参与者 durably blocked 且仅剩
定时器可推进时,由控制器推进 `bubble.now` 并把到期定时器作为调度点纳入枚举(roadmap 候选)。

---

## 9. 待定 / 开放问题

- 泡泡外并发的**显式检测报错** `uncontrolled concurrency detected`:如何在 synctest 跨泡泡检测
  基础上覆盖"共享地址被泡泡内外同时触碰"。
- 基于 `weavewrite` 钩子的 **undo-log 自动重置**:回滚粒度、只覆盖插桩内存的边界、开销。
- 弱内存(atomic C11 重排 / read-from 枚举)与 DPOR 的组合,复杂度可控性待验证。
- 状态空间预算/超时的默认策略与用户可调项;是否提供并行探索(多 worker 跑不同子树)。
- 受控 bubble 内假时钟推进的融合(见 D11 未来)。
