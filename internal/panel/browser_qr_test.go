package panel

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

func TestQoderSSOAlipayURL(t *testing.T) {
	before := time.Now().Add(9 * time.Minute).UnixMilli()
	raw := qoderSSOAlipayURL()
	after := time.Now().Add(11 * time.Minute).UnixMilli()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse SSO URL: %v", err)
	}
	if u.Host != "account.aliyun.com" || u.Path != "/sso/login.htm" {
		t.Fatalf("unexpected SSO endpoint: %s", raw)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "qoder",
		"redirect_uri":  "https://qoder.com.cn/sso/callback/aliyun",
		"return_url":    "https://qoder.com.cn",
		"response_type": "code",
		"scope":         "openid profile",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	exp, err := strconv.ParseInt(q.Get("expireTime"), 10, 64)
	if err != nil || exp < before || exp > after {
		t.Errorf("expireTime = %q, want within 10 minutes", q.Get("expireTime"))
	}
	if len(q.Get("nonce")) < 16 || len(q.Get("state")) < 16 {
		t.Errorf("nonce/state too short: %q %q", q.Get("nonce"), q.Get("state"))
	}
	if q.Get("nonce") == q.Get("state") {
		t.Error("nonce and state should differ")
	}
	if second := qoderSSOAlipayURL(); second == raw {
		t.Error("consecutive SSO URLs should differ")
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
