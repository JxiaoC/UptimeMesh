package probe

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/blues/jsonata-go"
	"github.com/uptimemesh/shared/checkconfig"
)

// JSON 断言:与 UptimeKuma 的 "HTTP(s) - JSON 查询" 完全同语义
// (见其 src/util.js 的 evaluateJsonQuery):
//
//  1. 响应体先尝试 JSON.parse;解析失败则按字符串对待(所以 "$" 能拿到非 JSON 的原始响应);
//  2. 用 JSONata 表达式取值(jsonPath 为空则直接用响应本身);
//  3. 取值为 null/undefined、数组、对象(含 Date/函数)都算失败 —— 断言只接受原始值;
//  4. 把取值与期望值都转成字符串,按运算符比较:
//     ==/!= 直接比较字符串,contains 判包含,>,>=,<,<= 先转数字再比较。
//
// 全程不 panic、不改动响应体;任何求值错误都返回可直接展示的中文原因。

// jsonAssertResult 是一次 JSON 断言的判定结果。
type jsonAssertResult struct {
	// Value 是取值转成的字符串(失败时也尽量给出,便于提示)。
	Value string
	// OK 断言是否通过。
	OK bool
	// Err 非空表示断言无法完成(表达式错误、取值类型不合法等),此时 OK=false。
	Err error
}

// evalJSONAssert 执行一次 JSON 断言。jsonPath 为空时不做断言,返回零值 + ok=false。
func evalJSONAssert(body []byte, jsonPath, operator, expected string) (res jsonAssertResult, enabled bool) {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "" {
		return jsonAssertResult{}, false
	}

	// 1. 能解析成 JSON 就解析,否则按原始字符串处理(与 Kuma 一致)。
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		doc = string(body)
	}

	// 2. JSONata 取值。
	expr, err := jsonata.Compile(jsonPath)
	if err != nil {
		return jsonAssertResult{Err: fmt.Errorf("JSON 查询表达式无法解析: %v", err)}, true
	}
	value, err := expr.Eval(doc)
	if err != nil {
		if err == jsonata.ErrUndefined {
			return jsonAssertResult{Err: fmt.Errorf("JSON 查询没有结果:请检查表达式与响应结构")}, true
		}
		return jsonAssertResult{Err: fmt.Errorf("JSON 查询求值失败: %v", err)}, true
	}

	// 3. 只接受原始值(字符串/数字/布尔)。
	switch v := value.(type) {
	case nil:
		return jsonAssertResult{Err: fmt.Errorf("JSON 查询结果为空:请检查表达式与响应结构")}, true
	case []any:
		return jsonAssertResult{Err: fmt.Errorf(
			"JSON 查询返回了数组(%s),断言需要单个值:可用 [0] 取首个元素,或 $count()/$sum()/$boolean() 之类的聚合",
			truncate(mustJSON(v), 60))}, true
	case map[string]any:
		return jsonAssertResult{Err: fmt.Errorf(
			"JSON 查询返回了对象(%s),无法与期望值比较:请在表达式里取到具体字段",
			truncate(mustJSON(v), 60))}, true
	}

	actual := valueToString(value)
	op := normalizeOperator(operator)

	// 4. 转字符串后比较。
	ok, err := compareJSONValue(actual, op, expected)
	if err != nil {
		return jsonAssertResult{Value: actual, Err: err}, true
	}
	return jsonAssertResult{Value: actual, OK: ok}, true
}

// normalizeOperator 归一运算符,空值按相等处理。
func normalizeOperator(op string) string {
	op = strings.TrimSpace(op)
	if op == "" {
		return checkconfig.OperatorEqual
	}
	return op
}

// compareJSONValue 按运算符比较取值与期望值。
func compareJSONValue(actual, operator, expected string) (bool, error) {
	switch operator {
	case checkconfig.OperatorEqual:
		return actual == expected, nil
	case checkconfig.OperatorNotEqual:
		return actual != expected, nil
	case checkconfig.OperatorContains:
		return strings.Contains(actual, expected), nil
	case checkconfig.OperatorGT, checkconfig.OperatorGTE, checkconfig.OperatorLT, checkconfig.OperatorLTE:
		a, err := strconv.ParseFloat(strings.TrimSpace(actual), 64)
		if err != nil {
			return false, fmt.Errorf("JSON 查询结果 %q 不是数字,无法用 %s 比较", truncate(actual, 40), operator)
		}
		b, err := strconv.ParseFloat(strings.TrimSpace(expected), 64)
		if err != nil {
			return false, fmt.Errorf("期望值 %q 不是数字,无法用 %s 比较", truncate(expected, 40), operator)
		}
		switch operator {
		case checkconfig.OperatorGT:
			return a > b, nil
		case checkconfig.OperatorGTE:
			return a >= b, nil
		case checkconfig.OperatorLT:
			return a < b, nil
		default:
			return a <= b, nil
		}
	default:
		return false, fmt.Errorf("不支持的比较方式 %q,可选:%s", operator, strings.Join(checkconfig.JSONAssertOperators, " / "))
	}
}

// valueToString 把 JSONata 取值转成字符串(与 Kuma 的 response.toString() 对齐):
// 整数不带小数点,布尔为 true/false,字符串原样。
func valueToString(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case bool:
		return strconv.FormatBool(n)
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1e15 {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case json.Number:
		return n.String()
	default:
		return fmt.Sprint(n)
	}
}

// ValidateJSONAssert 校验监控保存时的 JSON 断言配置:表达式必须能编译、
// 运算符必须在白名单内。返回中文错误原因,空字符串表示通过。
func ValidateJSONAssert(jsonPath, operator string) string {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "" {
		return ""
	}
	if _, err := jsonata.Compile(jsonPath); err != nil {
		return "JSON 查询表达式无法解析: " + err.Error()
	}
	op := normalizeOperator(operator)
	for _, allowed := range checkconfig.JSONAssertOperators {
		if op == allowed {
			return ""
		}
	}
	return "不支持的比较方式 " + op + ",可选:" + strings.Join(checkconfig.JSONAssertOperators, " / ")
}

// mustJSON 把值序列化成紧凑 JSON,仅用于错误提示。
func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
