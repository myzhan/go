# weave 路线图

状态图例:✅ 完成 · 🚧 进行中 · ⬜ 未开始

每个里程碑都可独立验证。原则:先把引擎(L3/L4)在纯 Go 里做扎实,再逐步把插桩底层从
"库级专用类型"换成"运行时 + 编译器钩子",最终兑现零改写。各层对外契约
(`choose`/`event`/`Wait`)保持稳定,便于平滑替换。

> ⚠️ **编号说明(2026-07 校对)**:本文件的 M0–M6 是**早期规划编号**,与 `README.md` 快照里的
> M0–M9 **不是同一套**(含义也不同,如本文 M3=atomic、README M3=跨 run 枚举)。**权威的当前状态
> 以 `README.md` 的"当前状态(快照)"为准**;本文件下方各里程碑的 ✅/🚧/⬜ 已按实际代码校正。

## 实际进度速览(以代码为准)

**已落地**:DPOR(含差分健全性验证)· runtime 接入真实 `chan`/`select`/`sync.Mutex`/`RWMutex`/
`WaitGroup.Wait`(零改写,`func()` 契约)· `-weave` 内存读写插桩(继承 race 逃逸剪枝)·
`go test -weave` flag · 失败 seed 重放 · 源码行号 trace · **goroutine 创建位置图例** ·
select 确定化 · RNG(map/maphash)确定化 · spawned goroutine panic 捕获 ·
**`-weave` 与 `-race`/`-msan`/`-asan` 互斥保护**。

**真正未做(剩余工作,已对代码核实)**:
1. **atomic 插桩**(最大缺口):`sync/atomic` 目前既非调度点也不记录 → 用 atomic 的无锁代码探索不了。
   正解仿 `-race` 用 instrumented std 重建,工程量大,为免拖累全体 Go 程序 atomic 性能未草率合入。
2. **剩余同步原语记录**:`sync.Cond`(Wait/Signal/Broadcast)、`Once.Do`、`WaitGroup.Add/Done`
   (目前只有 `.Wait` 有钩子)。
3. **弱内存模型**(M5):atomic C11 重排 / read-from 枚举。
4. **DPOR-over-select-cases**:select 已确定化,但未枚举多个就绪 case。
5. **(D9)泡泡外并发显式检测** `uncontrolled concurrency detected` + **undo-log 跨 run 自动重置**。
6. **抢占看门狗**(死循环兜底)。
7. **UX**:显示读写值、覆盖率/截断/状态空间估计、超时预算、并行探索、context/http 示例。
8. **死锁泄漏清理**:未做(已知限制,与 synctest 一致,可接受)。

---

## M0 — 原型引擎(纯 Go) ✅

**目标**:在不改 Go 源码树的前提下,用库级插桩原语验证 L3 探索引擎 + L4 用户 API。

位置:`weaveproto/`(独立 module)。

- [x] 协作式串行调度器:任一时刻仅一个 goroutine 运行,同步操作处交还控制权
      (`sched.loop`/`schedPoint`/`block`)
- [x] 严格 ping-pong 握手(resume/yield 无缓冲 channel),保证无数据竞态、无需锁
- [x] 系统性交错枚举:穷举 DFS odometer(`nextPlan`),`maxRuns` 兜底
- [x] 库级插桩原语:`Value[T]`(读/写=调度点)、`Mutex`(锁/解锁=调度点+阻塞)
- [x] 死锁检测(全组阻塞)、goroutine 反应式 reap(`abortAll`)
- [x] 公共 API:`Test`/`Go`/`Wait`/`Assert`
- [x] 失败复现:`choices` 向量 → `EncodeSeed`/`DecodeSeed` → `Replay` / `WEAVE_REPLAY`
- [x] 用例集:丢更新(3 调度命中)、加锁(420 调度无误报)、死锁(17 调度命中)、
      重放一致、env 重放

**验收**:`cd weaveproto && go test -v` 全绿,4 类场景符合预期。✅

---

## M1 — DPOR 替换穷举 ✅

**目标**:在 M0 原型内,把穷举 DFS 换成 DPOR,同样用例调度数大幅下降,结果不变。

- [x] happens-before 判定:**用程序序做保守近似**(非逐对象向量钟,但保守=健全);记录每步
      transition(wid、op、地址、enabled 掩码)到 trace 缓冲
- [x] 冲突判定:同对象、至少一写、且跨参与者(`explore.go` `conflict`)
- [x] source-DPOR:回溯分析对每对冲突 transition 在竞争前驱调度点维护 backtrack 集合
- [x] 用 backtrack 集合驱动下一条 schedule,替换 odometer(`Explore`;odometer 保留在
      `exploreExhaustive` 里做等价性对拍)
- [ ] (可选)升级 optimal-DPOR(wakeup tree)进一步剪枝 —— **未做**
- [~] 兜底开关:已加**调度数预算**(`ExploreBudget`/`DefaultMaxSchedules`/`WEAVE_MAX_SCHEDULES`,
      超限标 `Truncated`);更选择性的**抢占计数上界**(CHESS 式 context bounding)仍未做

**验收**(已达成):
- [x] DPOR 调度数显著 < 穷举(丢更新 3 vs 12;channel 19 vs 35),无误报、complete
- [x] 丢更新/死锁仍被发现
- [x] 对拍:DPOR 终态集合 == 穷举终态集合(`TestDPORSoundnessSuite`/`TestDPOREquivalence`)

---

## M2 — 接入 runtime:真实 chan / sync.Mutex 零改写 ✅（API 实为 `func()`，非 `func(t)`；lockrank 复用 synctest 无需新增）

**目标**:落地 L2 控制调度器,让**未改写**的真实 `chan`/`sync.Mutex`/`WaitGroup`/`Cond`
被探索。需改 Go 源码树并 `./make.bash` 重编工具链。

- [x] `synctestBubble` 加 `controlled` + `weaveCtl`
- [x] 新增 `src/runtime/weave.go`:控制器状态机 + run-token 串行化 + linkname 钩子
- [x] `ready` 在 controlled 下改走 runnable 集合(`weaveEnqueue`);`park_m`/`goexit`/`Yield` 交接令牌
- [x] linkname 接口:`weaveRunSchedule`(`internal/weave.runSchedule`)、`weaveYield`、`weaveWait`
      (命名与规划的 `weave_choose/event` 不同,但职责等价)
- [x] chan.go / select.go / sema(经 `internal/sync`)接 `weaveSchedPoint` + 让出令牌
- [x] `src/internal/weave/`:L3 引擎(`Explore`/`Replay`),经 linkname 接收回调
- [x] `src/testing/weave/`:`Test` + `Wait`(**签名是 `func()` 非 `func(t)`** —— 见 D-open,故意用无 t
      契约提示"可重跑")
- [~] lockrank:**复用 synctest 的 `lockRankSynctest`,未新增锁 → 无需登记**(N/A)

**验收**(已达成):
- [x] 真实 `sync.Mutex` + 普通变量写的用例行为与 M1 一致
- [x] `go test runtime sync internal/synctest testing/synctest` 无回归
- [x] 失败可 seed 重放

---

## M3 — 编译器插桩:atomic 纳入调度点 ⬜（**唯一大缺口**；内存插桩已先行完成，见 M4）

**目标**:落地 L1,让 `sync/atomic` 成为调度点,无需换类型。

- [ ] 新增构建模式 `-weave`(仿 `-race`:`cmd/go` flag + `-gcflags=-d=weave`)
- [ ] 编译器 pass 在被测包(及依赖,递归)插入 `runtime.weaveatomic(op, addr, ...)`
      (参考 `cmd/compile/internal/ssagen` 的 race 插桩)
- [ ] `runtime.weaveatomic` 接入 L2 控制器:调度点 + 事件上报
- [ ] 排除 runtime/sync 自身等不该插桩的包(仿 race 的 norace 处理)

**验收**:atomic 版丢更新、无锁栈/计数器的竞态在 `-weave` 下被发现;正确的无锁代码无误报。

---

## M4 — 共享内存读写插桩(高强度档) ✅（已完成，且早于 M3 atomic；即 `-weave` 的 `weaveread`/`weavewrite`）

**目标**:`-weave=race`,把普通内存读写也变调度点,让 DPOR 枚举数据竞态维度。

- [x] 编译器插入 `runtime.weaveread/weavewrite`(及 range 版),复用 race 的 `instrument2` 插入点
- [x] L3 对内存地址做冲突/happens-before 判定(程序序保守近似)
- [x] 访问过滤:继承 race 的 `IsSanitizerSafeAddr`(跳过不逃逸栈局部/只读全局/零大小);`-weave` 开关

**验收**:无锁共享变量的竞态被逐访问级别发现;开销/规模在可接受范围。

---

## M5 — 弱内存模型建模 ⬜

**目标**:像 loom 一样对 atomic 做 C11 重排枚举(relaxed load 可返回旧值)。

- [ ] `weaveatomic` load 由 L3 从该地址允许的历史写集合中选值返回
- [ ] 按 `doc/go_mem.html` 约束建模 acquire/release/relaxed 可见性
- [ ] 与 DPOR 结合的读值枚举(read-from 关系)

**验收**:仅在弱内存下才出现的重排 bug 被发现;顺序一致代码无误报。

---

## M6 — 工程化与体验 🚧（部分完成）

- trace 可读性:
  - [x] 源码行号(`weavePC` + `CallersFrames` → `file:line`)
  - [x] goroutine 创建位置图例(`gN` → `go` 语句处 + 函数名)
  - [ ] 合并冗余 wait/`run` 步
  - [ ] 显示读写的值(目前只显示地址)
- 覆盖率/进度报告:
  - [x] 已探索 schedule 数(`Result.Runs` → `explored N schedule(s)`)
  - [x] **是否截断** —— `Result.Truncated`/`TruncatedReason`;预算超限与容量溢出(`outcome==2`)都标注;
        `testing/weave` 截断时报 `INCOMPLETE` 并 `t.Errorf`(不再静默当 "ok")
  - [ ] 状态空间估计
- 超时/预算控制、并行探索:
  - [x] 调度数预算(`DefaultMaxSchedules` + `ExploreBudget` + `WEAVE_MAX_SCHEDULES` 环境变量覆盖)
  - [ ] wall-clock 超时
  - [ ] 并行探索(多 worker 跑不同子树)
- 文档 + 示例:
  - [x] 介绍性博客(`blog.md`)
  - [ ] context/http 等可运行 example(参考 testing/synctest 的 example)
- [x] 与 `go test` 集成:`-weave` flag + 失败输出/`WEAVE_REPLAY` 复现格式

---

## 里程碑依赖

```
M0 ✅ ──► M1(DPOR) ──► M2(runtime chan/mutex) ──► M3(atomic) ──► M4(内存) ──► M5(弱内存)
                                                    └──────────► M6(体验,可并行)
```

M1 与 M2 相对独立:M1 纯算法(在原型里做),M2 纯运行时接入。可并行推进,但建议先 M1
把引擎打磨好,再在 M2 把打磨好的引擎移植进 `internal/weave`。
