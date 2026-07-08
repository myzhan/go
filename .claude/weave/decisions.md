# weave 决策记录

轻量 ADR:记录重要设计决策及其理由,便于后续迭代时理解"为什么这样"而不重复讨论。

---

## D1 — 命名为 `weave`

**决策**:项目命名 `weave`(编织),而非沿用 loom。
**理由**:短、小写、动词,契合 Go 命名习惯(`sort`/`race`/`sync`/`synctest`);隐喻精准
(goroutine=线,探索交错=编织);与 `synctest` 天然成对;标准库/flag 无占用。
备选:`braid`、`interleave`。
**影响**:包 `testing/weave`·`internal/weave`,runtime `runtime/weave.go`,编译档 `-weave`,
钩子 `weave_choose`/`weave_event`,插桩函数 `runtime.weaveatomic`。

---

## D2 — 深度复用 synctest bubble,而非另造隔离机制

**决策**:weave 建在现有 `synctest` bubble 之上,不是正交的独立机制。
**理由**:bubble 已提供 goroutine 分组(创建时继承,`proc.go:5401`)、全组 durably-blocked
静止检测(`changegstatus`)、假时钟、对象归属。weave 只需补"调度顺序控制 + 串行化 + 跨 run
枚举"。`controlled` 字段加在 `synctestBubble` 上 = weave 是 bubble 的一种模式。
**含义**:`synctest` = bubble(隔离+假时钟);`weave` = bubble + 调度控制层。二者分层依赖,
非正交;唯一接近正交的是假时钟(可独立取舍)。

---

## D3 — 零改写靠 runtime + 编译器,而非 loom 式换类型

**决策**:让未改写的真实类型可被探索,而非要求用户改用 `weave.Mutex` 等专用类型。
**理由**:换类型是 Rust loom 最大工程学负担(须改整条依赖链)。Go 里:
- chan/mutex/select 本就是调用进 runtime 的函数 → runtime 钩子直接拦真实类型(L2)。
- atomic/裸内存是编译器 intrinsic / load-store 无调用点 → 编译器插桩(L1,类比 `-race`)。
bubble 归属创建时继承,跨 package 自动生效,对被测代码透明。
**代价**:必须动 runtime 与 compiler(用户已明确接受)。
**边界**:仅"从 bubble 内派生"的 goroutine 被控制;进程级预先存在的(init/单例/全局池)不在
其内,需在闭包内构造或用 fake。系统 goroutine(`proc.go:5397`)永不入 bubble。

---

## D4 — 探索引擎目标 DPOR,原型先用穷举占位

**决策**:最终用 DPOR;M0 原型先用穷举 DFS odometer 打通端到端,M1 再替换。
**理由**:用户要求"一步到位 DPOR",但 DPOR 实现复杂、易错,不适合作为"先看看"的第一版。
穷举完备且易验证,能立刻证明引擎/API/复现能力;`choose`/`event` 契约与 DPOR 一致,可平滑替换。
**代价**:原型阶段调度数偏大(如加锁用例 420 条),M1 后由偏序剪枝大幅下降。

---

## D5 — 只在同步操作处切换,不逐指令

**决策**:调度点 = 同步操作(chan/mutex/atomic/goroutine 起止),不在任意指令处切换。
**理由**:同 loom/CHESS。逐指令交错状态空间不可行;非原子内存竞态本就是 race detector 的职责。
需要更细粒度时用 L1 内存插桩档(M4)按访问切换。
**含义**:weave 查"某合法交错下程序对不对";race detector 查"有没有未同步的并发访问"。二者互补。

---

## D6 — 原型里为何用 `NewValue` 而非普通 `int`

**决策**:M0 原型中共享变量用 `weave.Value[T]`(`Get`/`Set`),而非裸 `int`。
**理由**:库级插桩下,调度器只能在"回调进调度器的操作"处切换。裸 `x = x + 1` 是几条 load/store,
无钩子、不可观测,调度器无法在读与写之间切走 → 丢更新的坏交错根本生成不出来。`Value.Get/Set`
把访问变成函数调用 = 可观测的调度点。这与 loom 强制换类型同因。
**去向**:这是**原型层妥协的占位符**。M2(runtime 钩子)让真实 chan/mutex 免包装;M3/M4(编译器
插桩)让普通 `int`/atomic 的访问自动变调度点,源码零改。届时用户视角 `NewValue` 消失。

---

## D7 — 失败复现用选择向量 seed

**决策**:失败交错编码为调度选择序列 `choices []int` → seed 字符串,支持 `Replay` /
`WEAVE_REPLAY` 确定性重放。
**理由**:失败时序完全由调度器各决策点的选择决定;捕获该向量即可百分百重放。便于写回归、贴同事。
**前提**:模型除 goroutine 调度外确定(无 rand/真实时间/map 迭代序依赖);改被测代码 seed 失效。
**实现**:`weaveproto/weave.go` 的 `EncodeSeed`/`DecodeSeed`/`Replay`。

---

## D8 — L2 始终编译、纯运行时门控(不用 build tag);普通程序成本 ≈ synctest

**最终决策(已修订)**:L2 的 runtime 钩子**始终编译进去、无 build tag**,`go test` 直接可跑
weave 测试(用户要求把 weave 测试当普通测试)。成本靠**运行时门控**:每个钩子先判
`gp.bubble != nil && controlled`,普通程序(无 bubble)只多一次**可预测的 not-taken 分支**,
footprint 与 synctest 一致(synctest 本身也是始终编译、靠 `gp.bubble != nil` 判断)。
**为何 L2 不需要 build tag(而 -race 需要)**:-race 在**每次内存访问**插桩(遍布、无法廉价
运行时门控),故必须编译期开关。L2 钩子只在 **park_m / ready / newproc / goexit** 这些**非每指令**
的调度choke point,数量少、可廉价运行时门控 → 无需 tag。
**曾经的中间态**:一度做过 `weaveenabled` const + `weave_on.go`/`weave_off.go` 的 build-tag DCE
方案(像 -race)。后按用户要求去掉 tag,合并为单个始终编译的 `runtime/weave.go`。
**L1 仍需 build 模式**:未来的编译器内存/atomic 插桩(L1)遍布每次访问,和 -race 同类,**那部分
必须**用 `-weave` 构建档;但 L2 与之解耦,始终可用。
**验证**:`go test internal/weave`(**无 tag**)全绿;synctest/sync + runtime(chan/select/sema/
sched/synctest/goroutine)零回归。

## 待定 / 开放问题

- L4 API 的 `t` 参数:目标形态 `func(t *testing.T)`,原型暂用无参 + `Assert`。M2 接入时统一。
- `weave.Wait` 与 `synctest.Wait` 的关系:能否直接复用,还是需 controlled 变体。
- 弱内存(M5)与 DPOR 的 read-from 枚举如何组合,复杂度可控性待验证。
- 状态空间预算/超时的默认策略与用户可调项。
- 是否提供并行探索(多 worker 跑不同子树),与 runtime 单 bubble 的关系。
