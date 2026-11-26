package panel

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
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
