# weave — Go 并发交错穷举测试框架

`weave` 让一个单元测试能够**系统性地遍历多 goroutine 的交错执行**,从而在没有 `-race`、没有
sleep、完全确定的前提下,稳定复现"只在特定调度下才出现"的并发 bug(丢更新、死锁、破坏不变量、
goroutine 泄漏、原子性违背),并给出**可复现的最小反例 + seed**。

目标定位:比 Rust `loom` 更贴合 Go——让**未经改写**的真实 `chan` / `sync.Mutex` / `select` /
普通变量代码就能被探索(靠运行时钩子 + 编译器插桩,而非强制换类型)。它是**单元级、封闭
(hermetic)的并发验证器**;对标 loom/Coyote/Shuttle/CHESS,同类工具的公认边界(fake 掉真实
I/O 与时钟)同样适用。

## 文档索引

| 文件 | 内容 |
|---|---|
| [arch.md](arch.md) | **整体架构**:四层结构、端到端数据流、关键组件与边界、构建形态 |
| [design.md](design.md) | **设计文档**:设计原则、各层设计、健全性论证、与 -race 关系、ADR 决策记录、范围边界 |
| [imp.md](imp.md) | **实现与状态**:文件/符号映射、关键运行时机制、里程碑状态、已知坑、验证方法 |
| [proposal.md](proposal.md) | 初始提案 |

## 当前状态

已达到**可用里程碑**:未改写的真实 `chan`/`select`/`sync.Mutex`/`RWMutex`/`WaitGroup`/`Cond`/
`Once` + 普通内存(`-weave`)代码可被系统性交错探索;DPOR(含差分健全性验证 + 抢占上界)、
select-case 枚举、非阻塞 channel 操作、RNG/map 序确定化、panic 捕获、失败 seed 重放 + 源码行号 +
goroutine 图例 + 读写值显示均已落地。**唯一大缺口**:`sync/atomic` 插桩(无锁代码探索不了)。

详细能力清单与里程碑见 [imp.md](imp.md) §1、§6。

## 快速跑通

```sh
# 核心引擎(无 -weave;chan/mutex/select 等已是调度点)
cd src && ../bin/go test internal/weave/ testing/weave/

# 含普通内存插桩的健全性套件
cd src && ../bin/go test -weave internal/weave/

# 演示模块(部分用例为故意失败,以展示 weave 抓 bug + 打印 seed)
cd weavedemo && ../bin/go test -weave -v
```

失败时打印逐步交错 + goroutine 图例 + 一个 seed,例如:

```
reproduce with: WEAVE_REPLAY=0.0.0.0.1.1.0.0.0.0.0 go test -run TestLostUpdate -v
```

用该 seed 可确定性重放逐字节相同的交错。

## 用户如何写测试

```go
func TestCounter(t *testing.T) {
    weave.Test(t, func() {               // testing/weave,真实类型、零改写
        var x int
        var mu sync.Mutex
        go func() { mu.Lock(); x++; mu.Unlock() }()
        go func() { mu.Lock(); x++; mu.Unlock() }()
        weave.Wait()                     // 等其余参与者退出后再断言
        if x != 2 { t.Errorf("x = %d, want 2", x) }
    })
}
```

约束(同 loom/synctest):模型闭包必须**可重跑**(共享状态在闭包内声明)、除调度外**确定性**
(勿用 `rand`/真实时间/依赖 map 迭代序)、所有 goroutine 必须能结束。时间/IO 用可被 weave 探索的
**内存接缝**建模(channel/select/`net.Pipe`/`context.WithCancel`);`time.Sleep`/定时器/
`context.WithTimeout` 在受控 bubble 内不可用(会误报死锁,见 [design.md](design.md) D11)。
