// fold_test.go R82 预检大小写不敏感回归。
//
// 输入一律由十六进制字节序列构造，原因见 AGENTS.md R82：本仓库的字符串字面量
// 曾经被工具静默改写（连字符丢失、大小写被拉平），用 % x 风格的数字构造可以让
// 测试用例与「源码里写成什么样」彻底解耦。
package sanitize

import (
	"encoding/hex"
	"testing"
)

// fromHex 解码一段十六进制，失败即 t.Fatal —— 常量写错要在测试里炸出来，
// 而不是静默变成空串让断言通过。
func fromHex(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return string(b)
}

// TestAsciiContainsFoldBasics 直接锁住 helper 本身的语义。
func TestAsciiContainsFoldBasics(t *testing.T) {
	lowerCCS := fromHex(t, "636c6175646520636f6465") // "claude code"
	upperCCS := fromHex(t, "434c4155444520434f4445") // "CLAUDE CODE"
	mixedCCS := fromHex(t, "436c6175646520634f6465") // "Claude cOde"

	if !asciiContainsFold(mixedCCS, lowerCCS) {
		t.Fatal("mixed-case input must match lowercase needle")
	}
	if !asciiContainsFold(lowerCCS, upperCCS) {
		t.Fatal("lowercase input must match uppercase needle")
	}
	if !asciiContainsFold(upperCCS, lowerCCS) {
		t.Fatal("uppercase input must match lowercase needle")
	}
	// 空 needle 恒真（与 strings.Contains 一致）
	if !asciiContainsFold(lowerCCS, "") {
		t.Fatal("empty needle must always match")
	}
	// needle 比 haystack 长 → 假
	longNeedle := fromHex(t, "636c6175646520636f6465212121")
	if asciiContainsFold(lowerCCS, longNeedle) {
		t.Fatal("needle longer than haystack must not match")
	}
}

// TestHasFingerprintCaseInsensitive 这是本次加固的核心用例：
// 特征表里的条目无论原文大小写如何组合，预检都必须命中。
// 旧实现用 strings.Contains（大小写敏感），小写变体会直接漏出、整条净化被跳过。
func TestHasFingerprintCaseInsensitive(t *testing.T) {
	// 取表中一个真实存在的条目做源：不用手写字面量，防止tableant方块被改写
	if len(featHeaders) == 0 {
		t.Fatal("featHeaders must not be empty")
	}
	for i, f := range featHeaders {
		if len(f) < 4 {
			continue // 跳过极短项与可能残缺的项
		}
		// 构造全小写 / 全大写两种变体
		var lowerB, upperB []byte
		for j := 0; j < len(f); j++ {
			c := f[j]
			if c >= 0x41 && c <= 0x5a {
				lowerB = append(lowerB, c+0x20)
				upperB = append(upperB, c)
			} else if c >= 0x61 && c <= 0x7a {
				lowerB = append(lowerB, c)
				upperB = append(upperB, c-0x20)
			} else {
				lowerB = append(lowerB, c)
				upperB = append(upperB, c)
			}
		}
		lower := "prefix " + string(lowerB) + " suffix"
		upper := "prefix " + string(upperB) + " suffix"
		if !hasFingerprint(lower) {
			t.Fatalf("featHeaders[%d] 全小写变体未命中预检: % x", i, lowerB)
		}
		if !hasFingerprint(upper) {
			t.Fatalf("featHeaders[%d] 全大写变体未命中预检: % x", i, upperB)
		}
	}
}

// TestHasFingerprintRejectsClean 干净文本不能被大小写折叠误伤 ——
// 预检变宽后最容易出的副作用就是假阳性。
func TestHasFingerprintRejectsClean(t *testing.T) {
	clean := fromHex(t, "48656c6c6f2c20706c65617365207772697465206120706f656d2e")
	if hasFingerprint(clean) {
		t.Fatalf("clean text must not trip precheck: % x", clean)
	}
}
