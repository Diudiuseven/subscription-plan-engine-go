package engine

import "time"

// proration 计算换套餐的补差/结余。
// 返回正数表示用户需补差（debit），负数表示产生结余留抵（credit）。
// 口径：按剩余时长占整周期比例折算，四舍五入到分；
// 同一口径可重放：剩余价值 = 整周期价 * 剩余时长 / 周期总时长。
func proration(oldPlan, newPlan Plan, now, periodStart, periodEnd time.Time) int64 {
	remaining := proratedValue(oldPlan, now, periodStart, periodEnd)
	cost := proratedValue(newPlan, now, periodStart, periodEnd)
	return cost - remaining
}

// proratedValue 某套餐在 [now, periodEnd] 区间的价值（分）。
func proratedValue(plan Plan, now, periodStart, periodEnd time.Time) int64 {
	total := periodEnd.Sub(periodStart)
	if total <= 0 {
		return 0
	}
	rem := periodEnd.Sub(now)
	if rem <= 0 {
		return 0
	}
	if rem > total {
		rem = total
	}
	// price * rem / total，四舍五入到分
	num := plan.PriceCents * int64(rem)
	den := int64(total)
	q := num / den
	r := num % den
	if r*2 >= den {
		q++
	}
	return q
}

// applyCredit 用结余抵扣应收金额，返回实收与剩余结余。
func applyCredit(amount, credit int64) (charge, remainingCredit int64) {
	if amount <= 0 {
		return 0, credit + (-amount) // 负应收转为结余
	}
	if credit >= amount {
		return 0, credit - amount
	}
	return amount - credit, 0
}
