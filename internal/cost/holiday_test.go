// 日期: 2026-11-14 14:05:31.546
// 作者: Zhai Wenjia

package cost

import (
	"testing"
	"time"
)

// 本文件覆盖峰谷判定与 cn-fixed 节假日日历（SDD 5.3）：
// 北京时间周一至周五 09:00–12:00 / 14:00–18:00 且非中国法定节假日 ⇒ 峰；
// 其余（周末 / 节假日 / 其他时段）⇒ 谷。

// TestIsCNFixedHoliday 钉住 cn-fixed 固定近似假期窗口（5.3：holiday_calendar=cn-fixed，
// 2026 + 2027 元旦 / 春节 / 清明 / 劳动节 / 端午 / 中秋 / 国庆）：窗口内日期（含两端）判 true，
// 窗口外判 false。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestIsCNFixedHoliday -v
func TestIsCNFixedHoliday(t *testing.T) {
	for d, want := range map[string]bool{
		// ---- 2026（5.3 固定节假日清单）----
		"2026-01-01": true,  // 元旦
		"2026-01-03": true,  // 元旦窗口尾日（含两端）
		"2026-01-04": false, // 窗口外
		"2026-02-17": true,  // 春节（初一 2026-02-17）
		"2026-04-05": true,  // 清明
		"2026-05-03": true,  // 劳动节
		"2026-06-19": true,  // 端午（五月初五 2026-06-19）
		"2026-09-25": true,  // 中秋（八月十五 2026-09-25）
		"2026-10-01": true,  // 国庆
		"2026-10-08": false, // 国庆窗口外
		"2026-11-16": false, // 普通工作日
		// ---- 2027 ----
		"2027-01-01": true,  // 元旦
		"2027-02-06": true,  // 春节（初一 2027-02-06）
		"2027-04-04": true,  // 清明
		"2027-05-01": true,  // 劳动节
		"2027-06-09": true,  // 端午（五月初五 2027-06-09）
		"2027-09-15": true,  // 中秋（八月十五 2027-09-15）
		"2027-10-01": true,  // 国庆
		"2027-11-16": false, // 日历外普通日
	} {
		ts, err := time.Parse("2006-01-02", d)
		if err != nil {
			t.Fatalf("样本日期 %s 解析失败：%v", d, err)
		}
		if got := isCNFixedHoliday(ts); got != want {
			t.Fatalf("isCNFixedHoliday(%s) = %v，期望 %v（5.3 cn-fixed）", d, got, want)
		}
	}
}

// TestIsPeakHour 钉住峰谷时段判定（5.3）：
// 周一至周五 09:00–12:00 / 14:00–18:00 且非节假日 ⇒ 峰；周末 / 节假日 / 其他时段 ⇒ 谷；
// 输入时区无关（换算到北京时间判定）。
// 时段边界取 [09:00,12:00) ∪ [14:00,18:00)（12:00 / 18:00 整点判谷）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestIsPeakHour -v
func TestIsPeakHour(t *testing.T) {
	for _, c := range []struct {
		name string
		t    time.Time
		want bool
	}{
		{"周一 09:00 峰", time.Date(2026, 11, 16, 9, 0, 0, 0, beijing), true},
		{"周一 10:00 峰", time.Date(2026, 11, 16, 10, 0, 0, 0, beijing), true},
		{"周一 11:59 峰", time.Date(2026, 11, 16, 11, 59, 0, 0, beijing), true},
		{"周一 12:00 谷（上窗口终点不含）", time.Date(2026, 11, 16, 12, 0, 0, 0, beijing), false},
		{"周一 13:59 谷（午休）", time.Date(2026, 11, 16, 13, 59, 0, 0, beijing), false},
		{"周一 14:00 峰", time.Date(2026, 11, 16, 14, 0, 0, 0, beijing), true},
		{"周一 17:59 峰", time.Date(2026, 11, 16, 17, 59, 0, 0, beijing), true},
		{"周一 18:00 谷（下窗口终点不含）", time.Date(2026, 11, 16, 18, 0, 0, 0, beijing), false},
		{"周一 08:59 谷", time.Date(2026, 11, 16, 8, 59, 0, 0, beijing), false},
		{"周一 20:00 谷", time.Date(2026, 11, 16, 20, 0, 0, 0, beijing), false},
		{"周六 10:00 谷（周末全天）", time.Date(2026, 11, 14, 10, 0, 0, 0, beijing), false},
		{"周日 10:00 谷（周末全天）", time.Date(2026, 11, 15, 10, 0, 0, 0, beijing), false},
		{"国庆周四 10:00 谷（法定节假日全天）", time.Date(2026, 10, 1, 10, 0, 0, 0, beijing), false},
		{"元旦周五 10:00 谷（法定节假日，工作日也谷）", time.Date(2027, 1, 1, 10, 0, 0, 0, beijing), false},
		// UTC 输入换算北京时间 10:30（周一）⇒ 峰（判定按 callTime 换算到北京时间，⛔ 不看输入时区）。
		{"UTC 02:30 = 北京 10:30 峰", time.Date(2026, 11, 16, 2, 30, 0, 0, time.UTC), true},
	} {
		if got := isPeakHour(c.t); got != c.want {
			t.Fatalf("%s：isPeakHour(%v) = %v，期望 %v（5.3 峰谷判定）", c.name, c.t, got, c.want)
		}
	}
}
