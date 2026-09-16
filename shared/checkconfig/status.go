package checkconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// StatusRangeSep 是期望状态码区间的展示分隔符,如 "400~499"。
// 解析时也接受半角/全角连字符,统一归一化为该符号。
const StatusRangeSep = "~"

// StatusCodeMin/Max 是状态码合法范围。
const (
	StatusCodeMin = 100
	StatusCodeMax = 599
)

// StatusSpecsFromCodes 把已展开的状态码还原为单码 spec,
// 用于兼容只存了 ExpectStatusCodes 的存量文档。
func StatusSpecsFromCodes(codes []int) []string {
	specs := make([]string, 0, len(codes))
	for _, c := range codes {
		specs = append(specs, strconv.Itoa(c))
	}
	return specs
}

// ParseStatusSpec 解析单个编辑态 spec:单码 "200" 或闭区间 "400~499"
// (也接受 "400-499")。返回归一化结果:区间两端相同则退化为单码。
func ParseStatusSpec(raw string) (string, error) {
	s := normalizeSpecText(raw)
	if s == "" {
		return "", fmt.Errorf("状态码不能为空")
	}
	if lo, hi, ok := splitStatusRange(s); ok {
		if err := checkStatusCode(lo); err != nil {
			return "", err
		}
		if err := checkStatusCode(hi); err != nil {
			return "", err
		}
		if lo > hi {
			return "", fmt.Errorf("状态码区间 %d~%d 起点大于终点", lo, hi)
		}
		if lo == hi {
			return strconv.Itoa(lo), nil
		}
		return strconv.Itoa(lo) + StatusRangeSep + strconv.Itoa(hi), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return "", fmt.Errorf("状态码 %q 非法,应为 100~599 的数字或 a~b 区间", raw)
	}
	if err := checkStatusCode(n); err != nil {
		return "", err
	}
	return strconv.Itoa(n), nil
}

// ExpandStatusSpecs 把 spec 列表展开为升序去重的状态码集合,供探测判定使用。
func ExpandStatusSpecs(specs []string) ([]int, error) {
	seen := map[int]bool{}
	for _, raw := range specs {
		spec, err := ParseStatusSpec(raw)
		if err != nil {
			return nil, err
		}
		if lo, hi, ok := splitStatusRange(spec); ok {
			for c := lo; c <= hi; c++ {
				seen[c] = true
			}
			continue
		}
		n, _ := strconv.Atoi(spec)
		seen[n] = true
	}
	out := make([]int, 0, len(seen))
	for c := StatusCodeMin; c <= StatusCodeMax; c++ {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// normalizeSpecText 去空白并统一全角字符,便于后续解析。
func normalizeSpecText(raw string) string {
	return strings.NewReplacer(
		"　", "",
		"～", StatusRangeSep,
		"－", "-",
		"—", "-",
	).Replace(strings.TrimSpace(raw))
}

// splitStatusRange 识别 "a~b" / "a-b" 区间;两端须为整数才视为区间。
func splitStatusRange(s string) (int, int, bool) {
	for _, sep := range []string{StatusRangeSep, "-"} {
		i := strings.Index(s, sep)
		if i < 0 {
			continue
		}
		a, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
		b, err2 := strconv.Atoi(strings.TrimSpace(s[i+len(sep):]))
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return a, b, true
	}
	return 0, 0, false
}

func checkStatusCode(n int) error {
	if n < StatusCodeMin || n > StatusCodeMax {
		return fmt.Errorf("状态码须在 %d~%d 之间", StatusCodeMin, StatusCodeMax)
	}
	return nil
}
