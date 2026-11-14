// 日期: 2026-11-14 11:54:49.172
// 作者: Zhai Wenjia

package cost

import "time"

// HolidayCalendarCNFixed 是默认节假日日历标识（统一 SDD 5.3：holiday_calendar 默认 cn-fixed）。
const HolidayCalendarCNFixed = "cn-fixed"

// beijing 是北京时间（UTC+8，无夏令时）。
var beijing = time.FixedZone("CST", 8*60*60)

// holidayRange 是一段连续假期（YYYY-MM-DD，含两端）。
type holidayRange struct{ start, end string }

// cnFixedHolidays 是中国法定节假日固定近似表（统一 SDD 5.3 holiday_calendar=cn-fixed）：
// 覆盖 2026 与 2027 两年的元旦 / 春节 / 清明 / 劳动节 / 端午 / 中秋 / 国庆。
//
// ⚠️ 这是**固定近似**：按各节日所在的近似假期窗口硬编码，⛔ 未含真实调休补班日
// （官方补班的周末在本表按周末判谷，可能与实际计费口径有出入）。
// 统一 SDD 5.3 允许「可配置替换为真实日历文件」——替换时只需改本表 / 换实现，调用方口径不变。
var cnFixedHolidays = []holidayRange{
	// ---- 2026 ----
	{"2026-01-01", "2026-01-03"}, // 元旦
	{"2026-02-15", "2026-02-22"}, // 春节（初一 2026-02-17）
	{"2026-04-04", "2026-04-06"}, // 清明
	{"2026-05-01", "2026-05-05"}, // 劳动节
	{"2026-06-19", "2026-06-21"}, // 端午（五月初五 2026-06-19）
	{"2026-09-25", "2026-09-27"}, // 中秋（八月十五 2026-09-25）
	{"2026-10-01", "2026-10-07"}, // 国庆
	// ---- 2027 ----
	{"2027-01-01", "2027-01-03"}, // 元旦
	{"2027-02-05", "2027-02-12"}, // 春节（初一 2027-02-06）
	{"2027-04-03", "2027-04-05"}, // 清明
	{"2027-05-01", "2027-05-05"}, // 劳动节
	{"2027-06-09", "2027-06-11"}, // 端午（五月初五 2027-06-09）
	{"2027-09-15", "2027-09-17"}, // 中秋（八月十五 2027-09-15）
	{"2027-10-01", "2027-10-07"}, // 国庆
}

// isCNFixedHoliday 判定北京时间日期是否落在 cn-fixed 固定近似假期窗口内。
func isCNFixedHoliday(t time.Time) bool {
	d := t.Format("2006-01-02")
	for _, r := range cnFixedHolidays {
		if d >= r.start && d <= r.end {
			return true
		}
	}
	return false
}

// isPeakHour 判定调用发起时间是否峰时（统一 SDD 5.3）：
// 北京时间周一至周五 09:00–12:00 / 14:00–18:00 且非中国法定节假日 ⇒ 峰时；
// 其余（周末 / 中国法定节假日 / 其他时段）⇒ 谷时。
// 判定用 callTime（调用发起时间），⛔ 不是响应完成时间（与官方按请求时段计费一致）。
// 时段取 [09:00,12:00) ∪ [14:00,18:00)（含起点不含终点；SDD 5.3 表述 9:00-12:00 / 14:00-18:00）。
func isPeakHour(callTime time.Time) bool {
	t := callTime.In(beijing)
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false // 周末全天谷时
	}
	if isCNFixedHoliday(t) {
		return false // 中国法定节假日全天谷时
	}
	h := t.Hour()
	return (h >= 9 && h < 12) || (h >= 14 && h < 18)
}
