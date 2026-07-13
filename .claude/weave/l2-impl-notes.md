# L2 实现笔记:精确注入点与串行化机制

调研 go1.25 源码后确定的 L2 落地细节。核心统一抽象:

> **每个调度点 = 一次显式的 `runtime.weaveSchedPoint(kind uint8, id unsafe.Pointer)` 调用。**
> 控制器在该调用内决定"当前 g 是否让出、下一个跑谁",实现串行化 + 交错枚举。
> 调度点由三种来源产生:runtime 阻塞路径、sync 包快速路径、编译器插桩(L1)。

## 为什么不能只靠 runtime 钩子

`sync.Mutex` 委托给 `internal/sync.Mutex`;**未竞争的 `Lock` 在 sync 包内做 atomic CAS 直接返回,
根本不进 runtime**(见 `sema.go:152` `cansemacquire` 快速返回;`sync/mutex.go:46` 委托)。
同理 atomic 是编译器 intrinsic。因此:

- **竞争/阻塞路径**:在 runtime(chan.go/sema.go/select.go 的 bubble 分支)加钩子——已有 bubble 判定。
- **非竞争快速路径**:在 `internal/sync` / `sync` 层入口加 `weaveSchedPoint` 调用(仅 controlled bubble 内生效)。
- **atomic / 裸内存**:编译器插桩(L1)。

## 注入点清单

| 层 | 文件 | 位置 | 动作 |
|---|---|---|---|
| runtime | `runtime2.go` | `synctestBubble` | 加 `controlled bool` + `ctl *weaveControl` 字段 |
| runtime | `runtime/weave.go`(新) | — | 控制器状态机 + linkname 钩子 `weave_choose`/`weave_event`/`weaveSchedPoint` |
| runtime | `proc.go:1133` `ready` | `runqput+wakep` | controlled 下改:不 runqput/wakep,交给控制器 enabled 集合 |
| runtime | `proc.go:457` `gopark` / `park_m` | 阻塞处 | controlled 下:上报 event,让控制器挑下一个 resume(复用 bubble.incActive 静止逻辑) |
| runtime | `proc.go:5401` `newproc1` | goroutine 创建 | controlled 下把新 g 注册进控制器 enabled 集合(bubble 已继承) |
| sync | `internal/sync/mutex.go` | `Lock`/`Unlock` 入口 | 若 in controlled bubble,调 `weaveSchedPoint(opLock/opUnlock, m)` |
| sync | `sync/runtime.go` | — | 加 linkname `runtime_weaveSchedPoint` 供 sync 层调用 |
| runtime | `chan.go` send(~280)/recv(~664)/close | bubble 分支 | 阻塞前/成功后 `weave_event(chanSend/Recv/Close, c)` |
| runtime | `select.go`(~200) | bubble 分支 | `weave_event(select, ...)` |
| runtime | `sema.go` | mutex 阻塞/唤醒 | `weave_event`;唤醒改走控制器 |
| runtime | `mklockrank.go` | — | 为新控制器锁登记 rank,重生成 `lockrank.go` |

## 零成本门控(已落地,见 [[decisions]] D8)

所有热路径调用点写成 `if weaveenabled { weaveSchedPoint(...) }`。`weaveenabled` 由 build tag
`weave` 门控:普通构建=false → 编译器 DCE 掉,零开销;weave 构建=true → 编进去,再由运行期
`weaveActive()`(`gp.bubble!=nil && controlled`)二次门控。文件:`runtime/weave.go`(共享)/
`weave_off.go`(!weave)/`weave_on.go`(weave)。控制器逻辑全部放 `weave_on.go`。

## 关键运行时机制(本轮调研确认)

- **新 goroutine 生下来就 park**:`newproc1(fn, callergp, pc, parked=true, waitreason)`(proc.go:5352)
  → g 建在 `_Gwaiting` 且**不入 runq**(proc.go:5425);由控制器 `goready` 唤醒。
  普通 `go` 语句走 `newproc`→`newproc1(parked=false)`+`runqput`+`wakep`(proc.go:5334)——
  controlled 下要改道:`if weaveenabled && weaveActive()` 时建 parked 并注册进控制器,不 runqput/wakep。
- **切换到别的 g 而不产生并行**:用 `gopark(unlockf, ...)`,在 unlockf 里 `goready(next)`。
  park_m 在把当前 g 切下 M 之后才调 unlockf(proc.go:4268),保证 next 只在 self 让出 M 后才可运行。
  不变量:任一时刻只有被选中的 g 可运行(其余都 park 在控制器里),故天然串行、无需禁 wakep 也不会并行。
- **控制器 park 不能被误判 durably-blocked**:`isIdleInSynctest`(runtime2.go:1381,查表 `isIdleInSynctest`)
  只有列在表里的 waitReason 才算 idle。控制器"等令牌"的 park 要用**不在该表里**的 waitReason
  (新增 `waitReasonWeaveScheduled`,不加进 idle 表),这样 bubble.running 不会因排队等令牌而下降;
  只有真实 durable block(chan/wg wait)才降 running、触发全组静止。

## 串行化机制(run-token)

controlled bubble 内维持不变量:**任一时刻至多一个 g 处于 `_Grunning`**。

1. 每个 bubble g 在 `weaveSchedPoint` / 阻塞点:上报事件 → 若控制器不选它 → `gopark`(park 到控制器队列)。
2. 控制器(跑在 root goroutine 或专用 g)从 enabled 集合按策略选一个 → `goready` 之。
3. `ready` 在 controlled 下**不 wakep**,避免第二个 P 并行跑另一个 bubble g。
4. 全组 durably-blocked 复用现有 `maybeWakeLocked`(synctest.go:132)= 一次 run 的静止/结束点。

> 本质与 `weaveproto` 原型的协作式调度器同构:原型用 resume/yield channel 握手,
> 真实版用 runtime 的 gopark/goready 握手;调度点从库级 Value/Mutex 换成上面这些真实钩子。
> 因此 **L3 决策逻辑(choose/event/DPOR/nextPlan)可直接从原型移植进 `internal/weave`**。

## 与引擎的接口(linkname 桥,仿 internal/synctest)

```
runtime → internal/weave(经 //go:linkname):
  weave_choose(enabled []int64) int64     // 选下一个 goid
  weave_event(op uint8, id uintptr, aux int64)  // 记 transition / 更新向量钟
  weave_runStart() / weave_runEnd()        // 每条 schedule 起止
internal/weave 持有:探索策略(odometer→DPOR)、happens-before 向量钟、trace、seed
testing/weave → internal/weave:驱动跨 run 循环、失败重放
```

## 分步验证(降低 runtime 改错风险)

1. **只加字段 + no-op 钩子**:`controlled` 字段 + `weaveSchedPoint` 空实现,重编,跑
   `go test runtime sync internal/synctest testing/synctest` 确认零回归。
2. **round-robin 串行化**:控制器固定顺序选 g,验证真实 `go func()` 在 controlled bubble 内
   严格一次跑一个(用计数器 + 无锁观察确定性)。
3. **接真实 sync.Mutex**:internal/sync 入口加 schedPoint,验证真实 Mutex 用例可被探索(去 weave.Mutex)。
4. **接 chan/select**,移植 L3 引擎,`testing/weave.Test` 可用。
5. 失败 seed 重放。

## 边界情况处理

分两类不同问题:

### A. goroutine 不经过调度点(死循环 / 纯计算)——会挂死

1. **忙等共享变量**(`for !ready {}`):被 L1/M4 内存插桩自然解决——每次读 `ready` 即调度点,
   控制器可切到对方置位。非特例。
2. **纯计算 / 忙等非共享**:**抢占看门狗**。controlled bubble 内复用 Go 异步抢占(sysmon 对超时 g
   发信号,在异步安全点打断);被抢占时路由进控制器作为**强制调度点**,保证任一 g 连续运行有上界。
   实现挂点:`preemptone`/`asyncPreempt` 回到调度器时,若 `bubble.controlled` 则交控制器。
3. **真·不终止**:每条 schedule 设**步数/时间预算**,超预算把该路径报成
   `possible livelock / non-termination`(附 trace),将"挂死"转为可定位失败。

### B. goroutine 离开受控世界(I/O / syscall / 真实时间)——会丢确定性

1. **真实时间**:bubble 有假时钟,但**受控 bubble 内假时钟推进被禁用**(`synctestRun1` 走
   `weaveRootWait`,跳过 synctest 的时钟推进循环)——故 `time.Sleep`/`time.After`/`NewTimer`/
   `context.WithTimeout` 在 `weave.Test` 内会永久 park、**误报死锁**。用内存接缝替代(channel/select/
   `net.Pipe`/`context.WithCancel`)。见 [[decisions]] D11、`weaveproto/asyncio_test.go`。
2. **真实网络/文件 I/O**:策略禁止,须用内存 fake(`net.Pipe`/假 clock),继承 synctest 规矩。
   **主动检测**:controlled bubble 内 `entersyscall`(`proc.go` `entersyscall`/`exitsyscall`)——
   - 良性短 syscall(alloc/栈增长/GC)容忍,`exitsyscall` 作为恢复进控制器的边界;
   - 无限期阻塞外部 I/O → 表现为"bubble 不静止且无可调度进展" → 报错
     `goroutine blocked on external I/O inside weave.Test — replace with an in-memory fake`。
3. **跨 bubble 通信**:保留 synctest 现有 `fatal`(chan.go 已有 `send/recv on synctest channel
   from outside bubble`)。

**原则**:死循环 → 抢占看门狗 + 步数预算(挂死转 livelock 报告);I/O → 假时钟 + fake + syscall
检测(不确定性转"请用 fake"错误)。

## 非确定性来源总表(全面梳理)

| 来源 | 处理 | 状态 |
|---|---|---|
| goroutine 交错 | 控制器串行化 + 枚举 | 做中 |
| `select` 多就绪 case | 走控制器决策(枚举+可重放),挂 selectgo `allSynctest` 分支 | task #8 |
| `runtime.Gosched`/手动让出 | 天然是调度点 | 自动 |
| map 迭代序 / `math/rand`(v2) / `hash/maphash` / 内部 `cheaprand` | controlled bubble 内按 run 确定化播种 cheaprand,一并变确定可复现 | task #9 |
| 真实时间 / timer | 假时钟(bubble 现成);**受控 bubble 内推进被禁用** → `time.Sleep`/timer 误报死锁,须用内存接缝(见 D11) | ⚠️ 受控内禁用 |
| I/O / syscall | fake + entersyscall 检测 | 见边界情况 |
| atomic / 内存弱序重排 | 弱内存建模 | M5(未来) |

### 已知问题(真难控,模型需规避)

- **GC / finalizer / cleanup 时序**:finalizer、`runtime.AddCleanup` 按设计跑在 bubble 之外的
  独立 goroutine 上(synctest 亦然),时序不可控、不可枚举。模型不得依赖。
- **sync.Pool**:内容取决于 GC 与 per-P 缓存,非确定;模型内避免。
- **指针地址相关**:ASLR / 分配器地址 / 打印指针 / 对裸指针 hash——不可控。
- **cgo / 外部进程**:超出 runtime 控制。

## 已知坑

- 快速路径钩子要极轻:非 controlled bubble 时必须是一次 `getg().bubble` 判空即返回(nosplit 友好)。
- 控制器自身用的锁/channel 不能又触发 weaveSchedPoint(递归);控制器代码需标记为"不受控"。
- GC/finalizer/系统 goroutine 不入 bubble(`proc.go:5397`),天然不受影响。
- timer/假时钟与 controlled 调度的交互:**已禁用**受控 bubble 内的假时钟推进(`weaveRootWait` 跳过
  推进循环),`time.Sleep`/定时器会误报死锁,用户须用内存接缝(channel/`net.Pipe`/`context.WithCancel`)。
  已实证并文档化(见 [[decisions]] D11、`weaveproto/asyncio_test.go`);后续再考虑融合。
