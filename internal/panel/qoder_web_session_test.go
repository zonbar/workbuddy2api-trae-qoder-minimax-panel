package panel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestQoderWebHasSessionCookie 保证「访客 Cookie」不会被当成已登录。
//
// 扫码前 qoder 域就已经有一批进站即有的 Cookie（cna / tfstk / acw_tc ...）。
// 早期版本只要看到任意 qoder 域 Cookie 就去打 /me/campaigns，于是扫码还没
// 完成就开始每 2.5 秒一次请求：未登录是 401、登录后被打成 429 —— 用户看到的
// 就是「查询活动失败 HTTP 429」「等待在浏览器中完成登录」来回跳。
func TestQoderWebHasSessionCookie(t *testing.T) {
	visitor := []cdpCookie{
		{Name: "cna", Value: "x", Domain: ".qoder.com.cn"},
		{Name: "isg", Value: "x", Domain: ".qoder.com.cn"},
		{Name: "tfstk", Value: "x", Domain: ".qoder.com.cn"},
		{Name: "acw_tc", Value: "x", Domain: "qoder.com.cn"},
		{Name: "arms_uid", Value: "x", Domain: "qoder.com.cn"},
		{Name: "qoder_locale", Value: "zh", Domain: "qoder.com.cn"},
		{Name: "qoder_visitor_id", Value: "x", Domain: "qoder.com.cn"},
	}
	if qoderWebHasSessionCookie(visitor) {
		t.Fatal("只有访客 Cookie 时不应判定为已登录")
	}
	// 其它域的 Cookie 不参与判定（阿里云 / 支付宝的访客态同理）。
	if qoderWebHasSessionCookie([]cdpCookie{{Name: "session", Value: "x", Domain: ".alipay.com"}}) {
		t.Fatal("非 qoder 域的 Cookie 不应参与判定")
	}
	withSession := append(append([]cdpCookie{}, visitor...),
		cdpCookie{Name: "qoder_session", Value: "abc", Domain: "qoder.com.cn"})
	if !qoderWebHasSessionCookie(withSession) {
		t.Fatal("出现访客之外的 Cookie 时应判定为可能已登录")
	}
}

// TestQoderWebMetaRoundTrip 保证网页会话的账号身份能落盘并读回。
//
// 只存 cookies 不存身份，界面就没法显示「加了哪个号」，自动签到也无法按号
// 报出「谁过期了」—— 这正是用户长期觉得「账号没添加成功」的原因之一。
func TestQoderWebMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := loadQoderWebMeta(dir); got.Email != "" || got.SessionKey != "" {
		t.Fatalf("没有 meta.json 时应返回零值，实际 %+v", got)
	}
	want := qoderWebMeta{
		Email: "someone@example.com", Name: "someone", UID: "u-1",
		SessionKey: "sso", APICN: true, SavedAt: "2026-11-27T18:00:00+08:00",
	}
	if err := saveQoderWebMeta(dir, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := loadQoderWebMeta(dir)
	if got.Email != want.Email || got.UID != want.UID || !got.APICN || got.SessionKey != want.SessionKey {
		t.Fatalf("读回不一致: %+v", got)
	}
	// 目录里没有 cookies.json 时，listQoderWebSessions 不应把它算作可用会话。
	if got := listQoderWebSessions(filepath.Dir(dir)); len(got) != 0 {
		t.Fatalf("无 cookies.json 不应计入会话: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "cookies.json"), []byte("[]"), 0o600); err != nil {
		t.Fatalf("write cookies: %v", err)
	}
	list := listQoderWebSessions(filepath.Dir(dir))
	if len(list) != 1 || list[0].Email != want.Email {
		t.Fatalf("有 cookies.json 时应列出该会话: %+v", list)
	}
	if list[0].Dir == "" {
		t.Fatal("列出会话时应带上目录名，自动签到要用它拼路径")
	}
}
