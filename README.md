# subscription-plan-engine-go

订阅生命周期与套餐变更引擎。同一账号在试用、升级、降级、取消、恢复之间切换时，
权益口径与计费结果自动对齐：无人工补差、无线下退款；套餐与价格按生效时间推进，
已出账的历史结果不受影响。

## 生命周期状态机

```
                StartTrial
                    │
                    ▼
              ┌──────────┐   ConvertTrial / 到期自动转正(ProcessDue)
              │ trialing │ ──────────────────────────────┐
              └──────────┘                               ▼
                   │                               ┌──────────┐  扣费失败
                   │                               │  active  │ ──────────▶ ┌──────────┐
                   │                               └──────────┘             │ past_due │ (宽限期内权益保留)
                   │                                    ▲    │              └──────────┘
                   │            Resume(撤销取消)          │    │ 重试+宽限耗尽        │ 重试成功
                   │                                    │    ▼                    ▼
                   │                                    │  ┌──────────┐     恢复 active
                   │                                    └──│suspended │ (挂起: 无权益, 欠费发票
                   │                                       └──────────┘     保持 open 继续重试)
                   │ Cancel(周期末生效)                         │ OnRetryExhausted=terminate
                   ▼                                            ▼
              ┌──────────┐
              │  ended   │  (终态)
              └──────────┘
```

### 关键语义

1. **试用衔接**：`StartTrial` 建立 trialing 订阅；`ProcessDue` 在 `TrialEnd` 时刻确定地
   自动转正，或 `ConvertTrial` 提前转化（付费周期从转化时刻起算；若试用已过期则从
   `TrialEnd` 起算）。状态翻转后重复触发是 no-op，不会产生第二次状态迁移。
2. **升降级补差**：按秒级精度对已用/剩余时长补差。`已用旧套餐 + 剩余新套餐` 与按整周期
   逐笔重算的口径严格一致（`UsedOld + Unused == 旧价全周期`，无舍入漂移）。正差额立即
   出账扣费；负差额记入 `CreditCents`，在后续周期自动留抵。
3. **生效时间**：`ChangePlan` 的 `EffectiveAt` 为未来时刻时只登记 `PendingChange`，
   不触碰当前套餐；到期前可用 `CancelScheduledChange` 撤销，撤销后回到原套餐且无中间
   状态残留。到期应用时按 **EffectiveAt**（而非处理时刻）计算补差。
4. **取消与终止**：`Cancel` 置 `CancelAtPeriodEnd`，周期内权益不变；`ProcessDue` 在周期
   末先检查取消标记再续费，因此取消与续费并发时结果确定：已取消的订阅绝不续费。
   `Resume` 在周期结束前可撤销取消。
5. **扣费失败**：失败后进入 past_due，按 `RetryPolicy.Intervals` 重试，宽限期
   （`GracePeriod`）内权益完整保留；重试与宽限耗尽后按 `OnRetryExhausted` 挂起
   （欠费发票保持 open，按末档间隔继续重试，可随时恢复）或终止。恢复只结算原发票，
   不重复累计欠费。
6. **并发与快照**：所有变更经单一互斥锁串行化，任意时刻 `GetSubscription` 读到的都是
   自洽快照（套餐、状态、权益、账单不会出现半生效）。所有写操作按 `RequestID` 幂等：
   重复提交返回当前状态；同一 `RequestID` 被复用于不同操作会被拒绝。
7. **重启恢复**：状态经 `Store` 持久化。重启后未完成的 `PendingChange` 保留原
   `EffectiveAt`，按下发时的语义继续推进；`ProcessDue` 由外部调度器（或测试时钟）驱动，
   恢复后从中断点继续。

## 权益口径

| 状态       | 权益 | 说明                         |
|------------|------|------------------------------|
| trialing   | ✅   | 试用期内完整权益             |
| active     | ✅   | 正常                         |
| past_due   | ✅   | 宽限期内权益保留             |
| suspended  | ❌   | 重试+宽限耗尽，可恢复        |
| ended      | ❌   | 终态                         |

## 配置项

```go
Config{
    GracePeriod:      72 * time.Hour,                    // 首次扣费失败后的宽限期
    Retry:            RetryPolicy{Intervals: []time.Duration{24h, 24h}}, // 每次重试的间隔
    OnRetryExhausted: ActionSuspend,                     // 或 ActionTerminate
}
```

- **套餐与价格**：`Catalog.UpsertPlan` 注册套餐（`Rank` 决定升/降级方向，`TrialDays`
  决定试用时长）；`Catalog.SetPrice` 注册按 `EffectiveFrom` 生效的价格版本。
  出账时取 `PriceAt(出账时刻)` 的价格并快照进发票行，历史发票永不回改。
- **时钟**：`Clock` 接口注入；生产用 `RealClock`，测试用 `ManualClock` 确定性推进。
- **支付网关**：`PaymentGateway` 接口注入，测试可模拟失败。
- **持久化**：`Store` 接口；`MemStore` 为参考实现（深拷贝进出）。

## 驱动方式

时间推进不依赖后台 goroutine：由调用方周期性调用 `ProcessDue()`（如每分钟一次
的定时任务），它按确定顺序处理：试用到期 → 到期的计划变更 → 周期末取消/续费 →
重试与宽限到期。测试通过 `ManualClock` + `ProcessDue` 获得完全确定的行为。

## 测试

```sh
go test -race ./...
```

覆盖：试用转化（自动/手动/幂等）、升降级补差与整周期对账、未来生效与撤销、
取消/恢复与取消-续费竞争、扣费失败重试与宽限、挂起恢复不重复计欠费、
并发重复提交与快照一致性、重启后未完成变更按原语义推进、价格版本按生效时间推进。
