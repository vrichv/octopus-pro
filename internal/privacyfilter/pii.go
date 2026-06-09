package privacyfilter

import (
	"regexp"
	"strings"
)

// 结构化 PII 正则。Go 的 regexp 是 RE2，不支持前后向断言，
// 因此数字边界用 digitBounded / ipBounded 在匹配后手工校验。
var (
	reEmail    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	rePhoneCN  = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9][0-9]{9}`)
	reIDCard   = regexp.MustCompile(`[1-9][0-9]{16}[0-9Xx]`)
	reBankCard = regexp.MustCompile(`[0-9]{13,19}`)
	reIPv4     = regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])`)
	reIPv6     = regexp.MustCompile(`(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|(?:[0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|(?:[0-9a-fA-F]{1,4}:){1,5}(?::[0-9a-fA-F]{1,4}){1,2}|(?:[0-9a-fA-F]{1,4}:){1,4}(?::[0-9a-fA-F]{1,4}){1,3}|(?:[0-9a-fA-F]{1,4}:){1,3}(?::[0-9a-fA-F]{1,4}){1,4}|(?:[0-9a-fA-F]{1,4}:){1,2}(?::[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:(?::[0-9a-fA-F]{1,4}){1,6}|:(?::[0-9a-fA-F]{1,4}){1,7}|(?:[0-9a-fA-F]{1,4}:){1,7}:|::`)
	reMAC      = regexp.MustCompile(`(?:[0-9a-fA-F]{2}[:\-]){5}[0-9a-fA-F]{2}`)
)

// 远程命令前缀。user@host 出现在这些命令上下文里通常是 SSH 目标，不是邮箱。
var sshCommands = []string{"ssh ", "scp ", "rsync ", "sftp ", "ssh-copy-id ", "ssh-keygen "}

// isInSSHCommandContext 检查 email 命中是否处于 ssh/scp/rsync 命令行里。
// 找到 email 所在行，看行内是否出现 ssh/scp/rsync 命令前缀（不限行首，
// 容忍 "打开 ssh user@host" 这种自然语言包裹）。
func isInSSHCommandContext(text string, emailStart int) bool {
	windowStart := emailStart - 128
	if windowStart < 0 {
		windowStart = 0
	}
	lineStart := strings.LastIndexByte(text[windowStart:emailStart], '\n') + 1 + windowStart
	line := text[lineStart:emailStart]
	for _, cmd := range sshCommands {
		if strings.Contains(line, cmd) {
			return true
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// digitBounded 校验匹配两侧不是数字（替代 RE2 没有的前后向断言）。
func digitBounded(text string, start, end int) bool {
	if start > 0 && isDigit(text[start-1]) {
		return false
	}
	if end < len(text) && isDigit(text[end]) {
		return false
	}
	return true
}

func isNetworkBytesCounter(text string, start int) bool {
	windowStart := start - 16
	if windowStart < 0 {
		windowStart = 0
	}
	prefix := strings.TrimRight(text[windowStart:start], " \t")
	return strings.HasSuffix(prefix, "bytes")
}

func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func isInLongHexByteSequence(text string, start, end int) bool {
	if end-start < 3 {
		return false
	}
	sep := text[start+2]
	if sep != ':' && sep != '-' {
		return false
	}
	if start >= 3 && text[start-1] == sep && isHexByte(text[start-2]) && isHexByte(text[start-3]) {
		return true
	}
	if end+3 <= len(text) && text[end] == sep && isHexByte(text[end+1]) && isHexByte(text[end+2]) {
		return true
	}
	return false
}

// ipBounded 校验匹配两侧不是数字或点。
func ipBounded(text string, start, end int) bool {
	if start > 0 && (isDigit(text[start-1]) || text[start-1] == '.') {
		return false
	}
	if end < len(text) && (isDigit(text[end]) || text[end] == '.') {
		return false
	}
	return true
}

func isIPv4Local(ip string) bool {
	groups := strings.Split(ip, ".")
	if len(groups) != 4 {
		return false
	}
	a := parseByte(groups[0])
	b := parseByte(groups[1])
	if a < 0 || b < 0 {
		return false
	}
	return a == 10 || a == 127 || a == 255 ||
		(a == 192 && b == 168) ||
		(a == 172 && b >= 16 && b <= 31)
}

func parseByte(s string) int {
	if s == "" || len(s) > 3 {
		return -1
	}
	v := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return -1
		}
		v = v*10 + int(s[i]-'0')
	}
	if v > 255 {
		return -1
	}
	return v
}

// isIPv6Local 判断 IPv6 是否为本地/私有地址，不应脱敏。
// 覆盖：::1（loopback）、::（unspecified）、fe80::/10（link-local）、fc00::/7（ULA）
func isIPv6Local(ip string) bool {
	low := strings.ToLower(ip)
	if low == "::1" || low == "::" {
		return true
	}
	if strings.HasPrefix(low, "fe80:") || strings.HasPrefix(low, "fc") || strings.HasPrefix(low, "fd") {
		return true
	}
	return false
}

// maskMAC 对 MAC 地址做部分脱敏，只保留前后 1 组。
// 7e:0b:23:8c:8b:30 → 7e:*:*:*:*:30
func maskMAC(mac string) string {
	var sep byte
	if strings.Contains(mac, "-") {
		sep = '-'
	} else {
		sep = ':'
	}
	groups := strings.Split(mac, string(sep))
	if len(groups) < 6 {
		return mac
	}
	return groups[0] + string(sep) + "*" + string(sep) + "*" + string(sep) + "*" + string(sep) + "*" + string(sep) + groups[5]
}

// maskIPv4 对 IPv4 做部分脱敏，只保留前后 1 组。
// 8.8.4.4 → 8.*.*.4
func maskIPv4(ip string) string {
	groups := strings.Split(ip, ".")
	if len(groups) < 4 {
		return ip
	}
	return groups[0] + ".*.*." + groups[3]
}

// maskIPv6 对 IPv6 做部分脱敏，只保留前后 1 组。
// 完整: 2001:0db8:85a3:0000:0000:8a2e:0370:7334 → 2001:*::*7334
// 压缩: 2001:db8::1 → 2001:*::*1
func maskIPv6(ip string) string {
	if strings.Contains(ip, "::") {
		// 展开 :: 为完整的 8 组
		parts := strings.SplitN(ip, "::", 2)
		leftGroups := splitGroups(parts[0])
		rightGroups := splitGroups(parts[1])
		padCount := 8 - len(leftGroups) - len(rightGroups)
		pad := make([]string, padCount)
		for i := range pad {
			pad[i] = "0"
		}
		all := append(leftGroups, pad...)
		all = append(all, rightGroups...)
		if len(all) < 8 {
			// 补齐到 8 组（极端情况）
			for i := len(all); i < 8; i++ {
				all = append(all, "0")
			}
		}
		return all[0] + ":*::*" + all[7]
	}
	groups := strings.Split(ip, ":")
	if len(groups) < 4 {
		return "*::*"
	}
	return groups[0] + ":*::*" + groups[len(groups)-1]
}

func splitGroups(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ":")
}

// luhnValid 做 Luhn 校验，过滤掉「长得像卡号的普通数字串」。
func luhnValid(num string) bool {
	sum := 0
	double := false
	for i := len(num) - 1; i >= 0; i-- {
		d := int(num[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// detectPII 返回结构化 PII 的命中区间。
func detectPII(text string) []span {
	var spans []span
	hasAt := strings.IndexByte(text, '@') >= 0
	hasDot := strings.IndexByte(text, '.') >= 0
	hasColon := strings.IndexByte(text, ':') >= 0
	hasDash := strings.IndexByte(text, '-') >= 0
	hasDigit := false
	for i := 0; i < len(text); i++ {
		if isDigit(text[i]) {
			hasDigit = true
			break
		}
	}
	if hasAt {
		for _, m := range reEmail.FindAllStringIndex(text, -1) {
			// SSH-style URL: user@host:path —— 邮箱后紧接 ":" + 非空白字符 → 视作 git@ URL，不脱
			if m[1] < len(text) && text[m[1]] == ':' &&
				m[1]+1 < len(text) && text[m[1]+1] != ' ' && text[m[1]+1] != '\t' {
				continue
			}
			// SSH 命令上下文：ssh / scp / rsync user@host 这种调用，host 不是邮箱
			if isInSSHCommandContext(text, m[0]) {
				continue
			}
			spans = append(spans, span{m[0], m[1], "[邮箱]"})
		}
	}
	if hasDigit {
		for _, m := range rePhoneCN.FindAllStringIndex(text, -1) {
			if digitBounded(text, m[0], m[1]) && !isNetworkBytesCounter(text, m[0]) {
				spans = append(spans, span{m[0], m[1], "[电话]"})
			}
		}
		for _, m := range reIDCard.FindAllStringIndex(text, -1) {
			if digitBounded(text, m[0], m[1]) {
				spans = append(spans, span{m[0], m[1], "[身份证]"})
			}
		}
		if hasDot {
			for _, m := range reIPv4.FindAllStringIndex(text, -1) {
				ip := text[m[0]:m[1]]
				if ipBounded(text, m[0], m[1]) && !isIPv4Local(ip) {
					spans = append(spans, span{m[0], m[1], maskIPv4(ip)})
				}
			}
		}
		for _, m := range reBankCard.FindAllStringIndex(text, -1) {
			if digitBounded(text, m[0], m[1]) && luhnValid(text[m[0]:m[1]]) {
				spans = append(spans, span{m[0], m[1], "[银行卡]"})
			}
		}
	}
	if hasColon {
		for _, m := range reIPv6.FindAllStringIndex(text, -1) {
			s, e := m[0], m[1]
			ip := text[s:e]
			// 排除 URL bracket 上下文：http://[::1]:8080/
			if s > 0 && text[s-1] == '[' {
				continue
			}
			// 排除本地/私有地址
			if isIPv6Local(ip) {
				continue
			}
			spans = append(spans, span{s, e, maskIPv6(ip)})
		}
	}
	if hasColon || hasDash {
		for _, m := range reMAC.FindAllStringIndex(text, -1) {
			if isInLongHexByteSequence(text, m[0], m[1]) {
				continue
			}
			mac := text[m[0]:m[1]]
			spans = append(spans, span{m[0], m[1], maskMAC(mac)})
		}
	}
	return spans
}
