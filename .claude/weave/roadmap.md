# weave 路线图

状态图例:✅ 完成 · 🚧 进行中 · ⬜ 未开始

每个里程碑都可独立验证。原则:先把引擎(L3/L4)在纯 Go 里做扎实,再逐步把插桩底层从
"库级专用类型"换成"运行时 + 编译器钩子",最终兑现零改写。各层对外契约
(`choose`/`event`/`Wait`)保持稳定,便于平滑替换。

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

## M1 — DPOR 替换穷举 🚧(下一步)

**目标**:在 M0 原型内,把穷举 DFS 换成 DPOR,同样用例调度数大幅下降,结果不变。

- [ ] 为每个对象/变量维护向量钟,记录 transition 的读/写与 happens-before
- [ ] 冲突判定:同对象、至少一写、且互不 happens-before
- [ ] source-DPOR:回溯时在竞争前驱调度点维护 backtrack 集合;先落地 source-set 版本
- [ ] 用 backtrack 集合驱动下一条 schedule,替换 `nextPlan` 的 odometer
- [ ] (可选)升级 optimal-DPOR(wakeup tree)进一步剪枝
- [ ] 抢占计数上界作为兜底开关

**验收**:
- 加锁递增用例调度数显著 < 420(偏序剪枝生效),且仍无误报、仍 complete
- 丢更新/死锁仍被发现;新增一个 3 goroutine 用例验证剪枝规模
- 对拍:小状态空间下 DPOR 找到的 bug 集合 == 穷举找到的集合(等价性回归)

---

## M2 — 接入 runtime:真实 chan / sync.Mutex 零改写 ⬜

**目标**:落地 L2 控制调度器,让**未改写**的真实 `chan`/`sync.Mutex`/`WaitGroup`/`Cond`
被探索。需改 Go 源码树并 `./make.bash` 重编工具链。

- [ ] `synctestBubble` 加 `controlled` 字段 + 控制器句柄(`src/runtime/synctest.go`)
- [ ] 新增 `src/runtime/weave.go`:控制器状态机 + run-token 串行化 + linkname 钩子
- [ ] `goready`/`ready` 在 controlled 下改走 enabled 集合(`proc.go`)
- [ ] `weave_choose`/`weave_event`/`weave_runStart/runEnd` linkname 接口
- [ ] chan.go/select.go/sema.go 的 bubble 分支接 `weave_event` + 让出令牌
- [ ] `src/internal/weave/`:把 M0/M1 的 L3 引擎移植进来,经 linkname 接收回调
- [ ] `src/testing/weave/`:`Test(t, func(t *testing.T))` + `Wait`,镜像 testing/synctest
- [ ] lockrank 登记新锁(`mklockrank.go` → 重生成 `lockrank.go`)

**验收**:
- 用**真实** `sync.Mutex` + 普通局部变量写的加锁/丢更新用例(chan 版)行为与 M1 一致
- `go test runtime sync internal/synctest testing/synctest` 无回归
- 失败仍能 seed 重放

---

## M3 — 编译器插桩:atomic 纳入调度点 ⬜

**目标**:落地 L1,让 `sync/atomic` 成为调度点,无需换类型。

- [ ] 新增构建模式 `-weave`(仿 `-race`:`cmd/go` flag + `-gcflags=-d=weave`)
- [ ] 编译器 pass 在被测包(及依赖,递归)插入 `runtime.weaveatomic(op, addr, ...)`
      (参考 `cmd/compile/internal/ssagen` 的 race 插桩)
- [ ] `runtime.weaveatomic` 接入 L2 控制器:调度点 + 事件上报
- [ ] 排除 runtime/sync 自身等不该插桩的包(仿 race 的 norace 处理)

**验收**:atomic 版丢更新、无锁栈/计数器的竞态在 `-weave` 下被发现;正确的无锁代码无误报。

---

## M4 — 共享内存读写插桩(高强度档) ⬜

**目标**:`-weave=race`,把普通内存读写也变调度点,让 DPOR 枚举数据竞态维度。

- [ ] 编译器插入 `runtime.weaveread/weavewrite(addr)`
- [ ] L3 用向量钟对内存地址做 happens-before / 竞态判定
- [ ] 提供开关与访问过滤(仅逃逸/共享对象),控制爆炸

**验收**:无锁共享变量的竞态被逐访问级别发现;开销/规模在可接受范围。

---

## M5 — 弱内存模型建模 ⬜

**目标**:像 loom 一样对 atomic 做 C11 重排枚举(relaxed load 可返回旧值)。

- [ ] `weaveatomic` load 由 L3 从该地址允许的历史写集合中选值返回
- [ ] 按 `doc/go_mem.html` 约束建模 acquire/release/relaxed 可见性
- [ ] 与 DPOR 结合的读值枚举(read-from 关系)

**验收**:仅在弱内存下才出现的重排 bug 被发现;顺序一致代码无误报。

---

## M6 — 工程化与体验 ⬜

- [ ] trace 可读性:合并冗余 wait 步、显示读写值、源码行号
- [ ] 覆盖率/进度报告:已探索 schedule 数、是否截断、状态空间估计
- [ ] 超时/预算控制、并行探索(多 worker 跑不同子树)
- [ ] 文档 + 示例(context/http 等,参考 testing/synctest 的 example)
- [ ] 与 `go test` 集成的 flag 与输出规范

---

## 里程碑依赖

```
M0 ✅ ──► M1(DPOR) ──► M2(runtime chan/mutex) ──► M3(atomic) ──► M4(内存) ──► M5(弱内存)
                                                    └──────────► M6(体验,可并行)
```

M1 与 M2 相对独立:M1 纯算法(在原型里做),M2 纯运行时接入。可并行推进,但建议先 M1
把引擎打磨好,再在 M2 把打磨好的引擎移植进 `internal/weave`。
