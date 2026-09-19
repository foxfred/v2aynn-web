package subscription

import (
	"strings"
	"testing"
)

// TestParseDoesNotPanicOnMalformedInput 用大量畸形输入轰击 Parse，断言它只会返回错误、绝不 panic。
//
// 为什么需要它：apiImportNode / apiAddNode 是**在持有配置锁的状态下**调用 Parse 的
// （见 server.go apiImportNode）。Parse 一旦 panic，那把锁就永远释放不掉，
// 整个服务（全部 API + 前端）彻底挂死，只能重启进程 —— 这正是 apiSettings 曾犯过的同类缺陷。
// 因此"Parse 对任意输入都不会 panic"是一个必须被守住的不变量，不能只靠肉眼看代码。
//
// 这里的输入全部是随手构造的畸形串：截断的链接、缺字段、空串、超长、错误协议名、
// 非法 base64、以及真实的订阅里可能遇到的怪东西。
func TestParseDoesNotPanicOnMalformedInput(t *testing.T) {
	bad := []string{
		"", " ", "\n", "\t", "://", "ss://", "vmess://", "vless://", "trojan://",
		"ss://@", "ss://:@", "ss://@:", "ss://a@b", "ss://a@b:", "ss://:b@c:1",
		"vmess://", "vmess://x", "vmess://!!!", "vmess://eyJ9",
		"vless://", "vless://@", "vless://a@b", "vless://a@b:99999",
		"trojan://", "trojan://@", "trojan://pw@", "trojan://pw@host", "trojan://pw@host:abc",
		"unknown://whatever", "http://example.com", "ftp://a@b",
		"ss://YWJj", "ss://****", "ss:////////", "ss://@@@@",
		"vmess://" + strings.Repeat("A", 10000),
		"trojan://" + strings.Repeat("%", 500) + "@h:1",
		strings.Repeat("\n", 100),
		"ss://\x00\x01\x02",
		"vless://a@b:443?type=tcp&security=",
		"vmess://eyJ2IjoiMiIsInBzIjoiIn0=",     // 合法 JSON 但字段缺失
		"vmess://eyJ2IjoiMiIsInBzIjoiIn0",      // base64 缺 padding
		"vmess://eyJhZGRyZXNzIjpudWxsfQ==",     // address 为 null
		"vmess://eyJ2bmV4dCI6Im5vdGFzbGljZSJ9", // vnext 类型不对
		"vmess://W10=",                         // base64 解出空数组
	}

	for _, s := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) 发生 panic: %v", truncate(s), r)
				}
			}()
			_, _ = Parse(s)
		}()
	}
}

// TestParseXrayJSONDoesNotPanicOnMalformedInput 同上，针对 Xray JSON 订阅分支。
func TestParseXrayJSONDoesNotPanicOnMalformedInput(t *testing.T) {
	bad := []string{
		"", "{", "[]", "{}", "null", "0", "\"\"",
		`{"outbounds":null}`, `{"outbounds":0}`, `{"outbounds":"x"}`,
		`{"outbounds":[null,0,"x",[]]}`,
		`{"outbounds":[{"protocol":"vless"}]}`,
		`{"outbounds":[{"protocol":"vless","settings":{"vnext":null}}]}`,
		`{"outbounds":[{"protocol":"vless","settings":{"vnext":[null]}}]}`,
		`{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"users":null}]}}]}`,
		`{"outbounds":[{"protocol":"trojan","settings":{"servers":[{"port":"abc"}]}}]}`,
		`{"outbounds":[{"protocol":"trojan","settings":{"servers":[{"port":1e400}]}}]}`,
	}

	for _, s := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseXrayJSON(%q) 发生 panic: %v", truncate(s), r)
				}
			}()
			_, _ = parseXrayJSON(s)
		}()
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
