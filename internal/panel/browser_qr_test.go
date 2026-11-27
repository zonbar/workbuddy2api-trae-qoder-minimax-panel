package panel

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

// TestQoderSSOStartsFromSiteSignIn 锁住「SSO 必须从站点自己的登录页起步」。
//
// 2026-11-27 实测：自己拼 account.aliyun.com/sso/login.htm?client_id=qoder... 时，
// 阿里云照样出二维码、扫码也回调带回了 code，但
// https://qoder.com.cn/sso/callback/aliyun 直接吐 "Bad Request"，
// 一个会话 Cookie 都拿不到 —— 扫码登录永远「登录成功但没登录」。
// 正确起点是 qoder 自己的 /users/sign-in，由页面自己发起 SSO。
func TestQoderSSOStartsFromSiteSignIn(t *testing.T) {
	if !strings.HasPrefix(qoderWebSignInURL, "https://qoder.com.cn/") {
		t.Fatalf("SSO 起点必须是 qoder 自己的登录页，实际 %q", qoderWebSignInURL)
	}
	if strings.Contains(qoderWebSignInURL, "account.aliyun.com") {
		t.Fatal("不能直接把浏览器开到阿里云 SSO 页：回调会 Bad Request")
	}
}

func TestQoderWebAPIBaseMatchesLoginMode(t *testing.T) {
	if got := (&qoderWebState{sso: true}).apiBase(); got != qoderWebAPICN {
		t.Errorf("SSO session apiBase = %q, want %q", got, qoderWebAPICN)
	}
	if got := (&qoderWebState{}).apiBase(); got != qoderWebAPI {
		t.Errorf("normal session apiBase = %q, want %q", got, qoderWebAPI)
	}
}

func TestBrowserQRPrepareJSHandlesAliyunSSO(t *testing.T) {
	if !strings.Contains(browserQRPrepareJS, "account.aliyun.com/sso/login.htm") {
		t.Fatal("prepare JS should handle Alibaba Cloud SSO page")
	}
	if !strings.Contains(browserQRPrepareJS, "alipay") || !strings.Contains(browserQRPrepareJS, "支付宝") {
		t.Fatal("prepare JS should click the Alipay login entry")
	}
}

func TestBrowserQRDismissJSProtectsLoginDialog(t *testing.T) {
	for _, kw := range []string{"订阅权益", "立即下载", "inset-0", "手机号登录", "扫码登录"} {
		if !strings.Contains(browserQRDismissJS, kw) {
			t.Errorf("dismiss JS should reference %q", kw)
		}
	}
	// 实测点 × 会把登录弹窗一起关掉，清理只能靠隐藏，不能点关闭按钮。
	if strings.Contains(browserQRDismissJS, ".click()") {
		t.Error("dismiss JS must not click any close button")
	}
	if !strings.Contains(browserQRDismissJS, "display = 'none'") && !strings.Contains(browserQRDismissJS, "display='none'") {
		t.Error("dismiss JS should hide overlays via display:none")
	}
}

func TestBrowserQRPortRejectsUnknownPlatform(t *testing.T) {
	p := &Panel{}
	if _, err := p.browserQRPort("unknown", ""); err == nil {
		t.Fatal("unknown platform should fail")
	}
	if _, err := p.browserQRPort("qoder", ""); err == nil {
		t.Fatal("qoder without email should fail")
	}
}

func TestBrowserQRPortRequiresRunningBrowser(t *testing.T) {
	p := &Panel{}
	for _, platform := range []string{"trae", "doubao", "codearts"} {
		if _, err := p.browserQRPort(platform, ""); err == nil {
			t.Fatalf("%s without browser should fail", platform)
		}
	}
}

func encodeTestPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// 截图内容校验必须认出真二维码，并挡住礼盒插画 / 空白帧 / 整页截图这几类
// 曾经被误当成二维码发给用户的图。
func TestQRShotLooksLikeQR(t *testing.T) {
	real, err := qrcode.Encode("https://example.com/login?state=abc123", qrcode.Medium, 256)
	if err != nil {
		t.Fatalf("encode qrcode: %v", err)
	}
	if !qrShotLooksLikeQR(real) {
		t.Error("real QR code should pass the check")
	}

	// 模拟豆包礼盒插画：浅蓝渐变，几乎没有暗像素和黑白跳变。
	decor := image.NewRGBA(image.Rect(0, 0, 171, 164))
	for y := 0; y < 164; y++ {
		for x := 0; x < 171; x++ {
			decor.Set(x, y, color.RGBA{R: uint8(120 + x/3), G: uint8(170 + y/8), B: 240, A: 255})
		}
	}
	if qrShotLooksLikeQR(encodeTestPNG(t, decor)) {
		t.Error("decorative illustration should NOT pass the QR check")
	}

	// Qoder 支付宝二维码渲染前的纯白首帧。
	blank := image.NewRGBA(image.Rect(0, 0, 304, 241))
	for y := 0; y < 241; y++ {
		for x := 0; x < 304; x++ {
			blank.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	if qrShotLooksLikeQR(encodeTestPNG(t, blank)) {
		t.Error("blank frame should NOT pass the QR check")
	}

	if qrShotLooksLikeQR([]byte("not a png")) {
		t.Error("invalid image should NOT pass the QR check")
	}
	if qrShotLooksLikeQR(nil) {
		t.Error("nil image should NOT pass the QR check")
	}
}

// TestTrimQRShotKeepsOnlyCode 保证「卡片式二维码」截图被裁到二维码本体：
// 支付宝 / 阿里云 SSO 页面把二维码放在带标题和说明文字的大卡片里，整块发给
// 用户会变成「一张大图中间一个小码」，在弹窗里被压到看不清。
func TestTrimQRShotKeepsOnlyCode(t *testing.T) {
	code, err := qrcode.New("https://qoder.com/sso/probe-token", qrcode.Medium)
	if err != nil {
		t.Fatalf("build qrcode: %v", err)
	}
	const (
		cw, ch = 480, 420
		size   = 180
		ox, oy = 150, 150
	)
	canvas := image.NewRGBA(image.Rect(0, 0, cw, ch))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	// 卡片标题：若干黑块，模拟「用支付宝扫码登录」这类文字行。
	for i := 0; i < 7; i++ {
		x := 40 + i*26
		for y := 40; y < 58; y++ {
			for xx := x; xx < x+18; xx++ {
				canvas.Set(xx, y, color.Black)
			}
		}
	}
	src := code.Image(size)
	draw.Draw(canvas, image.Rect(ox, oy, ox+size, oy+size), src, src.Bounds().Min, draw.Src)

	raw := encodeTestPNG(t, canvas)
	out := trimQRShot(raw)
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	b := img.Bounds()
	// 原图 480×420；裁完应该贴着 180 的二维码（含静区与 8% 留白）。
	if b.Dx() >= cw || b.Dy() >= ch {
		t.Fatalf("未裁剪：%dx%d", b.Dx(), b.Dy())
	}
	if b.Dx() < 140 || b.Dy() < 140 || b.Dx() > 260 || b.Dy() > 260 {
		t.Fatalf("裁剪结果尺寸异常：%dx%d，期望接近 %d 见方", b.Dx(), b.Dy(), size)
	}
	// 裁完必须仍然是二维码（不能把本体裁掉一块）。
	if !qrShotLooksLikeQR(out) {
		t.Error("裁剪后应仍能通过二维码内容校验")
	}
	// 纯色图不该被改动（没有可裁的内容）。
	blank := image.NewRGBA(image.Rect(0, 0, 200, 200))
	draw.Draw(blank, blank.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	if got := trimQRShot(encodeTestPNG(t, blank)); !bytes.Equal(got, encodeTestPNG(t, blank)) {
		t.Error("纯色图不应被裁剪")
	}
}

// TestQoderRawCookieHeaderCoversHostOnly 保证 401 兜底能带上 host-only 会话 Cookie。
// Go 的 cookiejar 严格按标准只把 host-only Cookie 发给设置它的主机，可浏览器发给
// openapi 子域时是带的 —— 不补这个兜底就会出现「浏览器里登录了，接口却 401」。
func TestQoderRawCookieHeaderCoversHostOnly(t *testing.T) {
	all := []cdpCookie{
		{Name: "session", Value: "abc", Domain: "qoder.com.cn", Path: "/"},
		{Name: "dotted", Value: "d", Domain: ".qoder.com.cn", Path: "/"},
		{Name: "intl", Value: "i", Domain: ".qoder.com", Path: "/"},
		{Name: "ali", Value: "y", Domain: ".alipay.com", Path: "/"},
	}
	h := qoderRawCookieHeader(all, "https://openapi.qoder.com.cn")
	for _, want := range []string{"session=abc", "dotted=d"} {
		if !strings.Contains(h, want) {
			t.Errorf("缺少 %s（host-only 也要带）: %s", want, h)
		}
	}
	for _, bad := range []string{"intl=", "ali="} {
		if strings.Contains(h, bad) {
			t.Errorf("不该带其它域的 %s: %s", bad, h)
		}
	}
	if got := qoderRawCookieHeader(nil, "https://openapi.qoder.com.cn"); got != "" {
		t.Errorf("没有 cookie 时应返回空串，实际 %q", got)
	}
}

// TestQRURLKeyIgnoresQuery 保证 SSO 链接每轮的 nonce/state 不影响「已准备」判定，
// 否则轮询会反复点「支付宝」入口，把页面点得来回跳转、二维码永远出不来。
func TestQRURLKeyIgnoresQuery(t *testing.T) {
	a := qrURLKey("https://account.aliyun.com/sso/login.htm?nonce=1&state=aa")
	b := qrURLKey("https://account.aliyun.com/sso/login.htm?nonce=2&state=bb")
	if a != b {
		t.Fatalf("同一页面的不同 query 应视为同一页：%q vs %q", a, b)
	}
	if qrURLKey("https://account.aliyun.com/sso/login.htm") == qrURLKey("https://account.aliyun.com/sso/other.htm") {
		t.Fatal("不同路径不应相等")
	}
}
