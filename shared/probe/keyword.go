package probe

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// keywordCandidates 返回关键字匹配要尝试的文本视图:第 0 个永远是原始响应体;
// 响应体是合法 JSON 且含字符串转义时,再追加一个反转义视图。
//
// 背景(设计与验证见 .scratch/keyword-json-escape/spec.md):关键字检查此前只在原始响应体
// 字节上做子串匹配,而 HTTP 上的 JSON 响应常把非 ASCII 写成 \uXXXX 转义(服务端框架的常见
// 默认,如 Python json.dumps 的 ensure_ascii=True)。于是「人在会解析 JSON 的客户端里明明
// 看得到」的中文关键字在原始字节里根本不存在:监控每轮都判失败,原因却是"响应体缺少关键字",
// 看起来像目标真的坏了。线上实例 —— 目标返回
//
//	{"code": -9, "msg": "\u767b\u5f55\u5df2\u5931\u6548", "res": {}}
//
// 配的关键字是「登录已失效」,连续 1020 轮全部因此判失败。
//
// 现在的语义是"命中 = 原始视图命中 或 反转义视图命中":
//   - 原始视图永远参与,既有配置的行为不变(例如把 JSON 片段连空格原样写进关键字);
//   - 两个视图是**或**关系,所以对 ExpectContains 只可能把原本判失败的轮次救回;对
//     ExpectNotContains 则能把原本因转义而漏掉的禁止关键字拦住(这是修正漏报);
//   - 响应体不是合法 JSON(HTML/纯文本/截断的 JSON)或没有反斜杠时不追加第二个视图,
//     判定与改动前完全一致。
func keywordCandidates(body []byte) []string {
	cands := []string{string(body)}
	if decoded := decodeJSONEscapes(body); decoded != "" {
		cands = append(cands, decoded)
	}
	return cands
}

// containsKeyword 在任一文本视图里找到子串即算命中。
func containsKeyword(cands []string, keyword string) bool {
	for _, text := range cands {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

// keywordOK 判定关键字规则:ExpectContains 逐条命中、ExpectNotContains 逐条未命中才通过。
// 空串规则忽略(与历来的行为一致)。
func keywordOK(body []byte, contains, notContains []string) bool {
	cands := keywordCandidates(body)
	for _, s := range contains {
		if s != "" && !containsKeyword(cands, s) {
			return false
		}
	}
	for _, s := range notContains {
		if s != "" && containsKeyword(cands, s) {
			return false
		}
	}
	return true
}

// decodeJSONEscapes 把合法 JSON 响应体里的字符串转义还原成字符,得到"人把响应体解析后
// 看到的正文":排版(空白、键序、数字写法)一律原样保留,只动转义本身。
//
// body 不是合法 JSON、或压根不含反斜杠时返回空串 —— 调用方据此跳过反转义视图(空响应体
// 同理:它没有第二个视图可言)。合法 JSON 里反斜杠只会出现在字符串内部,故这里可以直接
// 逐字节扫描,不必真的解析结构。
func decodeJSONEscapes(body []byte) string {
	if len(body) == 0 || !bytes.ContainsRune(body, '\\') || !json.Valid(body) {
		return ""
	}
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' || i+1 >= len(body) {
			b.WriteByte(c)
			i++
			continue
		}
		switch n := body[i+1]; n {
		case '"', '\\', '/':
			b.WriteByte(n)
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'b':
			b.WriteByte('\b')
			i += 2
		case 'f':
			b.WriteByte('\f')
			i += 2
		case 'u':
			r, size, ok := decodeUnicodeEscape(body[i+2:])
			if !ok {
				// 无法还原(不是 4 位十六进制、或落单的代理项):原样保留这段转义,
				// 不凭空造出半个字符。
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteRune(r)
			i += 2 + size
		default:
			// JSON 里没有这种转义;既然 json.Valid 已通过,这里只是防御性兜底。
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// decodeUnicodeEscape 解析 \u 之后的转义:4 位十六进制,或一对代理项(如
// "\uD83D\uDE00")。返回解码出的字符、消耗的字节数(4 或 10)与是否解析成功。
// 返回 false 时调用方保留原始转义文本 —— 落单的代理项不是合法 UTF-8 字符。
func decodeUnicodeEscape(rest []byte) (r rune, size int, ok bool) {
	if len(rest) < 4 {
		return 0, 0, false
	}
	hi, ok := parseHex4(rest[:4])
	if !ok {
		return 0, 0, false
	}
	if !utf16.IsSurrogate(rune(hi)) {
		return rune(hi), 4, true
	}
	// 代理项必须成对出现:高半部分紧跟 \uXXXX 形式的低半部分。
	if len(rest) >= 10 && rest[4] == '\\' && rest[5] == 'u' {
		if lo, ok := parseHex4(rest[6:10]); ok {
			if r := utf16.DecodeRune(rune(hi), rune(lo)); r != utf8.RuneError {
				return r, 10, true
			}
		}
	}
	return 0, 0, false
}

// parseHex4 解析 4 位十六进制数(大小写均可)。
func parseHex4(b []byte) (uint32, bool) {
	var v uint32
	for _, c := range b {
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}
