# subscription-plan-engine-go

订阅生命周期与套餐变更引擎。覆盖试用、升级、降级、取消、恢复、扣费失败重试与宽限期，
所有状态迁移按生效时间确定性推进，已出账的历史结果不受后续变更影响。

## 生命周期状态机

```
                Subscribe(含试用)          Subscribe(无试用)
                      │                        │
                      ▼                        ▼
                 trialing ──到期转化──▶ active ◀──┐
                                        │  │     │ 结清欠费
                     取消(Cancel)        │  │     │
                                        ▼  ▼     │
              周期结束终止 ◀── canceled   past_due（宽限期内有权益）
              ┌─────────┐                │ 重试耗尽 / 宽限到期
              ▼         ▼                ▼
           expired   suspended ◀── (按 OnExhausted 策略)
```

### 权益口径（`Entitled(sub, t)`）

| 状态 | 权益 |
|---|---|
| trialing / active | 有 |
| canceled（已取消未到期） | 有，直到 CurrentPeriodEnd |
| past_due（宽限期内，t ≤ GraceUntil） | 有 |
| suspended / expired | 无 |

## 核心语义

1. **试用转化**：试用到期由 `Advance(t)` 确定性地转化为 active 并出首期账单；
   事件处理后即被消费，同一时刻重复 `Advance` 不会产生二次迁移（幂等）。
2. **升降级补差**：立即生效的变更按剩余时长占整周期比例折算
   `剩余价值 = 整周期价 × 剩余时长 / 周期总时长`（四舍五入到分）。
   升级为补差（debit，立即扣费），降级为结余（credit，后续周期自动留抵）。
   该口径可重放对账：实收总额恒等于逐段（套餐×时长）重算之和，见
   `TestDowngradeCreditAndReconcile`。
3. **未来生效与撤销**：`ChangePlan` 传入未来 `effectiveAt` 则登记 `PendingChange`，
   到期由 `Advance` 按生效时刻的剩余时长折算执行；到期前 `CancelPendingChange`
   撤销后回到原套餐，不产生任何补差/结余记录。
4. **取消与续费并发**：取消（`Cancel`）只设置周期结束终止标记，周期内权益不变；
   周期结束事件到达时取消标记优先于续费，结果确定：终止而非续费。
   `Resume` 在周期结束前可撤销取消。
5. **扣费失败**：失败后进入 past_due 并开启宽限期，按 `RetryIntervalDays` 重试；
   重试次数耗尽或宽限期到期（先到为准）后按 `OnExhausted` 策略挂起或终止。
   挂起期间欠费定格不累计；恢复（`RecordPaymentResult(true)`）结清欠费后
   回到 active，挂起恢复的订阅自恢复时刻起算新周期，不重复累计历史欠费。
6. **并发与快照**：引擎内所有变更在单把互斥锁下完成，`Get` 返回拷贝快照；
   任意时刻读到的都是某个自洽状态，不存在半生效中间态。
   `Subscribe`/`ChangePlan` 支持幂等键，重复提交返回首次结果。
7. **持久化与重启**：`Store` 接口有内存与 JSON 文件（`FileStore`）两种实现。
   `PendingChange` 随订阅一起持久化，重启后由 `Advance` 按下发时的
   生效时刻语义继续推进；已被撤销的变更不会残留。

## 配置项（`Config`）

| 字段 | 含义 |
|---|---|
| `GracePeriodDays` | 扣费失败后的宽限期天数，宽限期内保留权益 |
| `MaxRetries` | 最大自动重试次数 |
| `RetryIntervalDays` | 相邻重试间隔天数 |
| `OnExhausted` | 重试耗尽策略：`PolicySuspend`（挂起，可恢复）或 `PolicyTerminate`（终止） |

套餐（`Plan`）可配置：价格（分）、计费周期天数、试用天数、权益列表。
扣费通道通过 `SetCharger` 注入，便于对接支付网关或注入失败场景。

## 事件推进

引擎不内置后台定时器，由调用方周期性调用 `Advance(now)`：
按时间顺序消费每个订阅上到期的四类事件——试用结束、周期结束（续费/终止）、
未来变更生效、欠费重试。乱序或重复的推进调用均安全。

## 测试

```sh
go test ./engine/ -race
```

覆盖：试用转化幂等、升降级补差与对账、未来生效与撤销、取消/恢复/续费并发、
扣费失败重试与宽限期、挂起恢复不重复累计、并发读写快照一致性、重启后继续推进。
