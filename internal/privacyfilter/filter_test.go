package privacyfilter

import (
	"fmt"
	"strings"
	"testing"
)

func newFilter(t *testing.T) *Filter {
	t.Helper()
	f, err := NewFromBytes(GitleaksRules)
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	return f
}

func redact(t *testing.T, f *Filter, text string) string {
	t.Helper()
	return f.Redact(text).Redacted
}

// --- 结构化 PII ---

func TestEmail(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "有事联系 test.user@example.com 谢谢"); !strings.Contains(got, "[邮箱]") {
		t.Errorf("邮箱未脱敏: %q", got)
	}
}

func TestPhoneCN(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "我的手机是 13812345678 随时打"); !strings.Contains(got, "[电话]") {
		t.Errorf("手机号未脱敏: %q", got)
	}
}

func TestIDCard(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "身份证号 11010519900307743X 务必保密"); !strings.Contains(got, "[身份证]") {
		t.Errorf("身份证未脱敏: %q", got)
	}
}

func TestBankCardValidLuhn(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "付款卡号 4111111111111111"); !strings.Contains(got, "[银行卡]") {
		t.Errorf("银行卡未脱敏: %q", got)
	}
}

func TestBankCardInvalidLuhnIgnored(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "订单编号 1234567890123456"); strings.Contains(got, "[银行卡]") {
		t.Errorf("Luhn 不通过的数字串被误判成银行卡: %q", got)
	}
}

// --- 密钥层 ---

func TestGitleaksRulesLoaded(t *testing.T) {
	f := newFilter(t)
	rules, skipped := f.Stats()
	if rules <= 100 {
		t.Errorf("gitleaks 规则只加载了 %d 条（应上百条）", rules)
	}
	t.Logf("gitleaks 规则 %d 条，跳过 %d 条", rules, skipped)
}

func TestNewWithEmptyPathLoadsEmbeddedGitleaksRules(t *testing.T) {
	f, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rules, skipped := f.Stats()
	if rules <= 100 {
		t.Fatalf("New(\"\") loaded fallback rules only: rules=%d skipped=%d", rules, skipped)
	}
}

func TestContextPassword(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "我的密码是 Hunter2xyz"); !strings.Contains(got, "[密钥]") {
		t.Errorf("上下文口令未脱敏: %q", got)
	}
}

func TestContextAPIKey(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "配置里 api_key = aB3xK9pLmN2qR7sT"); !strings.Contains(got, "[密钥]") {
		t.Errorf("api_key 未脱敏: %q", got)
	}
}

func TestEntropyFallback(t *testing.T) {
	f := newFilter(t)
	if got := redact(t, f, "临时凭证 aB3xK9pLmN2qR7sT5vW1zY 已生成"); !strings.Contains(got, "[密钥]") {
		t.Errorf("高熵随机串未脱敏: %q", got)
	}
}

// --- 高熵兜底反误报 ---

// SSH 命令上下文里的 user@host 不当邮箱（允许命令前有自然语言前缀）。
func TestSSHCommandContextSkipsEmail(t *testing.T) {
	f := newFilter(t)
	cases := []string{
		"ssh user@host.example.com",
		"ssh -i ~/.ssh/id_rsa user@host.example.com",
		"打开 ssh user@host.example.com",
		"scp file.txt user@host.example.com:/data/",
		"rsync -av /src/ user@host.example.com:/dst/",
	}
	for _, in := range cases {
		if got := redact(t, f, in); strings.Contains(got, "[邮箱]") {
			t.Errorf("SSH 目标被误判成邮箱: in=%q got=%q", in, got)
		}
	}
}

// 反面：纯邮箱（无 ssh 命令前缀）仍要脱
func TestSSHCommandContextPlainEmailStillRedacted(t *testing.T) {
	f := newFilter(t)
	in := "我的邮箱是 alice@example.com 请保密"
	if got := redact(t, f, in); !strings.Contains(got, "[邮箱]") {
		t.Errorf("普通邮箱漏脱: %q", got)
	}
}

// 回归 case：ls 命令传入长路径时，路径片段被熵兜底误判成密钥。
func TestEntropyFallbackSkipsFilesystemPath(t *testing.T) {
	f := newFilter(t)
	in := "ls /home/user/AbCdEfGh1234567890XyZ"
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("文件路径被误判: %q", got)
	}
}

// URL path segment 不应被脱敏。
func TestEntropyFallbackSkipsURLPath(t *testing.T) {
	f := newFilter(t)
	in := "curl https://api.example.com/v1/users/AbCdEfGh1234567890XyZ"
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("URL 路径被误判: %q", got)
	}
}

// S3 / 对象存储路径。
func TestEntropyFallbackSkipsS3URI(t *testing.T) {
	f := newFilter(t)
	in := "aws s3 cp s3://my-bucket/dir/AbCdEfGh1234567890XyZ ."
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("S3 URI 被误判: %q", got)
	}
}

// 容器镜像 sha256 摘要。
func TestEntropyFallbackSkipsSha256Digest(t *testing.T) {
	f := newFilter(t)
	in := "docker pull registry.io/img@sha256:9f86d081884c7d659a2feaa0c55ad015b1b8a3e6b1d2c4a5e9f8b7d6c5a4b3210"
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("sha256 摘要被误判: %q", got)
	}
}

// 邮件附件 / 邮箱本身的 domain 段不应触发兜底（域名内 . 边界）。
func TestEntropyFallbackSkipsHostname(t *testing.T) {
	f := newFilter(t)
	in := "host=long-subdomain-with-many-chars.example.com"
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("主机名被误判: %q", got)
	}
}

// 有 token 关键词的命令行（候选串与关键词同段），仍然要脱敏。
func TestEntropyFallbackKeepsTokenWithContext(t *testing.T) {
	f := newFilter(t)
	in := "请使用 token aB3xK9pLmN2qR7sT5vW1zY 调试"
	if got := redact(t, f, in); !strings.Contains(got, "[密钥]") {
		t.Errorf("带 token 关键词的随机串未脱敏: %q", got)
	}
}

// 普通乱码无 context（既无密钥语义关键词，也无路径边界）走严格阈值。
// 22 字符全异熵 ≈ 4.46，不到 4.8 → 应被放过，避免误伤普通 ID。
func TestEntropyFallbackStrictThresholdSkipsModerateEntropy(t *testing.T) {
	f := newFilter(t)
	in := "订单编号 aB3xK9pLmN2qR7sT5vW1zY 已记账"
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("无密钥语义的中等乱度串不应脱敏: %q", got)
	}
}

// --- 强上下文凌驾路径检查 ---

// Bearer 真密钥含 base64 padding 的 / —— 不能被 Step 1 当路径放过。
func TestStrongContextOverridesPathBoundary_BearerWithSlash(t *testing.T) {
	f := newFilter(t)
	in := `Authorization: Bearer abcDEF1234567890/xyzABC4567890==`
	if got := redact(t, f, in); !strings.Contains(got, "[密钥]") {
		t.Errorf("Bearer 真密钥被路径检查放过: %q", got)
	}
}

// 反面：api_key 出现在域名里（不是赋值结构）→ 仍走路径检查 → 不脱
func TestStrongContextNotTriggeredByPathKeyword(t *testing.T) {
	f := newFilter(t)
	in := `api_key.example.com/AbCdEfGh1234567890XyZ`
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("api_key.example.com 域名段被误判: %q", got)
	}
}

// --- Post Validators ---

// 模板变量不脱
func TestPostValidatorSkipsTemplateVar(t *testing.T) {
	f := newFilter(t)
	cases := []string{
		`secret={{ API_KEY }} 或 token=${TOKEN}`,
		`config: token=%{TOKEN}`,
		`auth=<API_KEY>`,
	}
	for _, in := range cases {
		if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
			t.Errorf("模板变量被误判: in=%q got=%q", in, got)
		}
	}
}

// 业务 ID 不脱
func TestPostValidatorSkipsBusinessIDAssignment(t *testing.T) {
	f := newFilter(t)
	cases := []string{
		`order_id=aB3xK9pLmN2qR7sT5vW1zY`,
		`user_id=AbCdEfGh1234567890XyZQwErTyUiOp`,
		`session_no=aB3xK9pLmN2qR7sT5vW1zY1234`,
	}
	for _, in := range cases {
		if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
			t.Errorf("业务 ID 被误判: in=%q got=%q", in, got)
		}
	}
}

// UUID 不脱
func TestPostValidatorSkipsUUID(t *testing.T) {
	f := newFilter(t)
	in := `trace_id=550e8400-e29b-41d4-a716-446655440000`
	if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
		t.Errorf("UUID 被误判: %q", got)
	}
}

// 长度=32/40/64 的纯 hex hash 不脱（md5/sha1/sha256）
func TestPostValidatorSkipsHexHash(t *testing.T) {
	f := newFilter(t)
	cases := []string{
		`md5: 9f86d081884c7d659a2feaa0c55ad015`,
		`sha1: da39a3ee5e6b4b0d3255bfef95601890afd80709`,
		`commit 9f86d081884c7d659a2feaa0c55ad015b1b8a3e6b1d2c4a5e9f8b7d6c5a4b3210`,
	}
	for _, in := range cases {
		if got := redact(t, f, in); strings.Contains(got, "[密钥]") {
			t.Errorf("hex hash 被误判: in=%q got=%q", in, got)
		}
	}
}

// --- 整体行为 ---

func TestCleanTextNoHit(t *testing.T) {
	f := newFilter(t)
	text := "今天天气不错，我们一起去公园散步吧。"
	res := f.Redact(text)
	if res.Redacted != text || res.Hit || res.Count != 0 {
		t.Errorf("干净文本被误改: %+v", res)
	}
}

func TestMultipleEntities(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("邮箱 a@b.com 手机 13900001111 密码是 Qwer1234")
	if !strings.Contains(res.Redacted, "[邮箱]") ||
		!strings.Contains(res.Redacted, "[电话]") ||
		!strings.Contains(res.Redacted, "[密钥]") {
		t.Errorf("多实体脱敏不全: %q", res.Redacted)
	}
	if res.Count < 3 {
		t.Errorf("命中数 %d，应 >= 3", res.Count)
	}
}

func TestBuiltinFallback(t *testing.T) {
	f, err := New("") // 空路径 → 用内置兜底规则
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if rules, _ := f.Stats(); rules == 0 {
		t.Error("内置兜底规则为空")
	}
}

// --- IPv4 部分脱敏 ---
func TestIPv4Mask(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("server at 8.8.4.4")
	if !strings.Contains(res.Redacted, "8.*.*.4") {
		t.Errorf("IPv4 部分脱敏失败: %q", res.Redacted)
	}
}

func TestIPv4PrivateRangesSkipped(t *testing.T) {
	f := newFilter(t)
	input := "10.1.2.3 127.0.0.1 172.16.1.2 172.31.255.254 192.168.1.1 255.255.255.255"
	res := f.Redact(input)
	if res.Redacted != input {
		t.Errorf("常见内网/本地 IPv4 不应脱敏: %q", res.Redacted)
	}
}

func TestIPv4Public172OutsidePrivateRangeMasked(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("172.32.1.9")
	if !strings.Contains(res.Redacted, "172.*.*.9") {
		t.Errorf("172.32/12 外公网 IPv4 应脱敏: %q", res.Redacted)
	}
}

// --- IPv6 部分脱敏 ---
func TestIPv6Full(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("addr 2001:0db8:85a3:0000:0000:8a2e:0370:7334")
	if !strings.Contains(res.Redacted, "2001:*::*7334") {
		t.Errorf("IPv6 完整形式脱敏失败: %q", res.Redacted)
	}
}

func TestIPv6Compressed(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("server at 2001:db8::1")
	if !strings.Contains(res.Redacted, "2001:*::*1") {
		t.Errorf("IPv6 压缩形式脱敏失败: %q", res.Redacted)
	}
}

func TestIPv6HexOnlyMasked(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("addr cafe:babe::dead")
	if !strings.Contains(res.Redacted, "cafe:*::*dead") {
		t.Errorf("IPv6 无十进制数字时也应脱敏: %q", res.Redacted)
	}
}

func TestIPv6LoopbackSkipped(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("connect to ::1")
	if res.Hit {
		t.Errorf("::1 loopback 不应脱敏: %q", res.Redacted)
	}
}

func TestIPv6LinkLocalSkipped(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("fe80::1%eth0")
	if res.Hit {
		t.Errorf("fe80:: link-local 不应脱敏: %q", res.Redacted)
	}
}

func TestIPv6ULASkipped(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("fd12:3456:789a::1")
	if res.Hit {
		t.Errorf("fd ULA 不应脱敏: %q", res.Redacted)
	}
}

func TestIPv6InURLSkipped(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("http://[2001:db8::1]:8080/")
	if strings.Contains(res.Redacted, "*::*") {
		t.Errorf("URL bracket IPv6 不应脱敏: %q", res.Redacted)
	}
}

// --- sk- API Key 部分脱敏 ---
// --- sk- API Key 部分脱敏 ---
func TestSKKeyMask(t *testing.T) {
	f := newFilter(t)
	key := "sk-proj-abc123def456ghi789jkl012"
	res := f.Redact("key=" + key)
	// sk- 保留，最后 2 位 "12" 保留，中间全 *
	masked := maskSecret(key)
	if masked == "" {
		t.Fatalf("maskSecret 应返回非空: key=%q", key)
	}
	if !strings.Contains(res.Redacted, masked) {
		t.Errorf("sk- 部分脱敏失败: got %q, 期望含 %s", res.Redacted, masked)
	}
}

func TestSKKeyWithDash(t *testing.T) {
	f := newFilter(t)
	key := "sk-or-OC.-3123HNxNItR8fJVLDYTULFdIXqV"
	res := f.Redact("Bearer " + key)
	masked := maskSecret(key)
	if masked == "" {
		t.Fatalf("maskSecret 应返回非空: key=%q", key)
	}
	if !strings.Contains(res.Redacted, masked) {
		t.Errorf("sk- 带 . 和 - 部分脱敏失败: got %q, 期望含 %s", res.Redacted, masked)
	}
}

func TestGenericAPIQuestionDoesNotTriggerSecret(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("do I need an api? here is 1f2e3d4c5b6a7980")
	if res.Hit {
		t.Errorf("问句中的 api? 不应触发 generic-api-key: %q", res.Redacted)
	}
}

// --- MAC 地址部分脱敏 ---
func TestMACMask(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("ether 7e:0b:23:8c:8b:30")
	if !strings.Contains(res.Redacted, "7e:*:*:*:*:30") {
		t.Errorf("MAC 部分脱敏失败: %q", res.Redacted)
	}
}

func TestMACDash(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("00-1A-2B-3C-4D-5E")
	if !strings.Contains(res.Redacted, "00-*-*-*-*-5E") {
		t.Errorf("MAC dash格式部分脱敏失败: %q", res.Redacted)
	}
}

func TestMACLettersOnlyMasked(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("ether aa:bb:cc:dd:ee:ff")
	if !strings.Contains(res.Redacted, "aa:*:*:*:*:ff") {
		t.Errorf("MAC 无十进制数字时也应脱敏: %q", res.Redacted)
	}
}

func TestMACDashLettersOnlyMasked(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("ether aa-bb-cc-dd-ee-ff")
	if !strings.Contains(res.Redacted, "aa-*-*-*-*-ff") {
		t.Errorf("MAC dash 无十进制数字时也应脱敏: %q", res.Redacted)
	}
}

func TestNetworkBytesCountersAreNotPhoneNumbers(t *testing.T) {
	f := newFilter(t)
	res := f.Redact("RX packets 46371003  bytes 13144912395 (13.0 GB)")
	if strings.Contains(res.Redacted, "[电话]") {
		t.Errorf("流量 bytes 数值不应脱敏成电话: %q", res.Redacted)
	}
}

func TestUnspecHardwareAddressIsNotMAC(t *testing.T) {
	f := newFilter(t)
	line := "unspec 00-00-00-00-00-00-00-00-00-00-00-00-00-00-00-00  txqueuelen 500  (UNSPEC)"
	res := f.Redact(line)
	if res.Redacted != line {
		t.Errorf("UNSPEC 长硬件地址不应按 MAC 分段脱敏: %q", res.Redacted)
	}
}

// --- ifconfig 综合测试 ---
func TestIfconfig(t *testing.T) {
	f := newFilter(t)
	input := `enp0s3: flags=4163<UP,BROADCAST,RUNNING,MULTICAST>  mtu 9000
        inet 8.8.4.4  netmask 255.255.255.0  broadcast 8.8.4.255
        inet6 2611:c04:4502:9988::1a00  prefixlen 128  scopeid 0x0<global>
        inet6 fe80::17ff:fe00:766e  prefixlen 64  scopeid 0x20<link>
        ether 02:10:17:09:76:66  txqueuelen 1000  (Ethernet)
        RX packets 46371003  bytes 13000000000 (13.0 GB)`
	res := f.Redact(input)
	// IPv4: public address is masked, private netmask is retained.
	if !strings.Contains(res.Redacted, "8.*.*.4") || !strings.Contains(res.Redacted, "255.255.255.0") {
		t.Errorf("IPv4 脱敏/保留策略不符: %q", res.Redacted)
	}
	// IPv6 public: 2611:c04:4502:9988::1a00 → masked
	if strings.Contains(res.Redacted, "2611:c04:4502:9988::1a00") {
		t.Errorf("IPv6 公网地址未脱敏: %q", res.Redacted)
	}
	// IPv6 link-local: fe80::17ff:fe00:766e → 不脱敏
	if !strings.Contains(res.Redacted, "fe80::17ff:fe00:766e") {
		t.Errorf("fe80 link-local 不应脱敏: %q", res.Redacted)
	}
	// MAC: 02:10:17:09:76:66 → masked
	if strings.Contains(res.Redacted, "02:10:17:09:76:66") {
		t.Errorf("MAC 未脱敏: %q", res.Redacted)
	}
}

// --- ifconfig IPv6 多地址测试 ---
func TestIfconfigIPv6(t *testing.T) {
	f := newFilter(t)
	// 公网 IPv6 应脱敏, fe80/fd/::1 不脱敏
	res := f.Redact("inet6 2a11:ab3c:ff09:99::1\ninet6 fe80::7424:15ff:fe78:185\ninet6 fd89:dfea:5538::1\ninet6 ::1")
	if strings.Contains(res.Redacted, "2a11:ab3c:ff09:99::1") {
		t.Errorf("IPv6 公网未脱敏: %q", res.Redacted)
	}
	if !strings.Contains(res.Redacted, "fe80::7424:15ff:fe78:185") {
		t.Errorf("fe80 应保留: %q", res.Redacted)
	}
	if !strings.Contains(res.Redacted, "fd89:dfea:5538::1") {
		t.Errorf("fd ULA 应保留: %q", res.Redacted)
	}
	if !strings.Contains(res.Redacted, "::1") {
		t.Errorf("::1 loopback 应保留: %q", res.Redacted)
	}
}

// --- IPv6 full form with real data ---
func TestIPv6FullReal(t *testing.T) {
	f := newFilter(t)
	// de-ipv6 的完整 8 组地址
	res := f.Redact("2a11:ab3c:ff09:99:ff65:ff49:ff64:2")
	if strings.Contains(res.Redacted, "2a11:ab3c:ff09:99:ff65:ff49:ff64:2") {
		t.Errorf("IPv6 完整地址未脱敏: %q", res.Redacted)
	}
	masked := maskIPv6("2a11:ab3c:ff09:99:ff65:ff49:ff64:2")
	if !strings.Contains(res.Redacted, masked) {
		t.Errorf("IPv6 脱敏格式不符: got %q, 期望含 %s", res.Redacted, masked)
	}
}

// BenchmarkRedact 测不同文本长度下的脱敏耗时。
func BenchmarkRedact(b *testing.B) {
	f, err := NewFromBytes(GitleaksRules)
	if err != nil {
		b.Fatal(err)
	}
	unit := "我叫张伟，邮箱 a@b.com，密码是 Hunter2xy，卡号 4111111111111111。"
	for _, size := range []int{50, 2000, 32000, 256 * 1024, 1024 * 1024} {
		text := strings.Repeat(unit, size/len(unit)+1)[:size]
		b.Run(fmt.Sprintf("%dB", len(text)), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f.Redact(text)
			}
		})
	}
}

func BenchmarkRedactComponents(b *testing.B) {
	full, err := NewFromBytes(GitleaksRules)
	if err != nil {
		b.Fatal(err)
	}
	builtin, err := NewFromBytes(nil)
	if err != nil {
		b.Fatal(err)
	}
	unit := "我叫张伟，邮箱 alice@example.com，密码是 sk-testabcdefghijklmnopqrstuvwxyz12，卡号 4532015112830366。"
	text := strings.Repeat(unit, 1024*1024/len(unit)+1)[:1024*1024]
	b.Run("pii-only-1MiB", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = detectPII(text)
		}
	})
	b.Run("builtin-secret-1MiB", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = builtin.secrets.detect(text)
		}
	})
	b.Run("gitleaks-secret-1MiB", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = full.secrets.detect(text)
		}
	})
}

func BenchmarkPIIRules1MiB(b *testing.B) {
	unit := "我叫张伟，邮箱 alice@example.com，密码是 sk-testabcdefghijklmnopqrstuvwxyz12，卡号 4532015112830366。"
	text := strings.Repeat(unit, 1024*1024/len(unit)+1)[:1024*1024]
	b.Run("email", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reEmail.FindAllStringIndex(text, -1)
		}
	})
	b.Run("phone", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = rePhoneCN.FindAllStringIndex(text, -1)
		}
	})
	b.Run("idcard", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reIDCard.FindAllStringIndex(text, -1)
		}
	})
	b.Run("ipv4", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reIPv4.FindAllStringIndex(text, -1)
		}
	})
	b.Run("ipv6", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reIPv6.FindAllStringIndex(text, -1)
		}
	})
	b.Run("mac", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reMAC.FindAllStringIndex(text, -1)
		}
	})
	b.Run("bank", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = reBankCard.FindAllStringIndex(text, -1)
		}
	})
}

func BenchmarkRedactSparseText(b *testing.B) {
	f, err := NewFromBytes(GitleaksRules)
	if err != nil {
		b.Fatal(err)
	}
	unit := "普通日志行 status ok request completed without credentials or personal data.\n"
	for _, size := range []int{256 * 1024, 1024 * 1024} {
		text := strings.Repeat(unit, size/len(unit)+1)[:size]
		b.Run(fmt.Sprintf("%dB", len(text)), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f.Redact(text)
			}
		})
	}
}

func BenchmarkSecretSparseBreakdown(b *testing.B) {
	f, err := NewFromBytes(GitleaksRules)
	if err != nil {
		b.Fatal(err)
	}
	unit := "普通日志行 status ok request completed without credentials or personal data.\n"
	text := strings.Repeat(unit, 1024*1024/len(unit)+1)[:1024*1024]
	b.Run("lower", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = lowerForSearch(text)
		}
	})
	b.Run("rule-prefilter", func(b *testing.B) {
		low := lowerForSearch(text)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for j := range f.secrets.rules {
				if ruleApplies(&f.secrets.rules[j], low) {
					count++
				}
			}
			_ = count
		}
	})
	b.Run("detect", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = f.secrets.detect(text)
		}
	})
}
