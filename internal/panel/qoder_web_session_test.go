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

// TestQoderWebSessionsDedupeByAccount 锁住「同一个号只出现一行」。
//
// 2026-11-27 用户实测：扫码时留空邮箱（键 = sso）登了一次，后来又填邮箱
// 登了一次，data/qoder-web 下就有两个 profile 目录。两份 cookie 都有效、身份
// 也都能认出来，签到卡里于是出现两行一样的「账号 f73d31」—— 用户以为是
// 「重复添加」，而且没有任何地方能删。
func TestQoderWebSessionsDedupeByAccount(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, m qoderWebMeta) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cookies.json"), []byte("[]"), 0o600); err != nil {
			t.Fatalf("cookies: %v", err)
		}
		if err := saveQoderWebMeta(dir, m); err != nil {
			t.Fatalf("meta: %v", err)
		}
	}
	old := qoderWebMeta{
		Email: "someone@example.com", UID: "u-1", SessionKey: "sso",
		SavedAt:     "2026-11-27T20:05:34+08:00",
		LastCheckin: &qoderWebCheckinResult{Phase: "pending", At: "2026-11-27T20:05:34+08:00"},
	}
	fresh := qoderWebMeta{
		Email: "someone@example.com", UID: "u-1", SessionKey: "someone@example.com",
		SavedAt:     "2026-11-27T20:33:32+08:00",
		LastCheckin: &qoderWebCheckinResult{Phase: "claimed", Amount: 100, At: "2026-11-27T20:33:32+08:00"},
	}
	mk("aaaa00000001", old)
	mk("bbbb00000002", fresh)
	// 未识别身份的会话（没登录成功）不该被合并成一条。
	mk("cccc00000003", qoderWebMeta{SessionKey: "unidentified", SavedAt: "2026-11-27T20:00:00+08:00"})

	list := listQoderWebSessions(root)
	if len(list) != 2 {
		t.Fatalf("同一账号应合并成一行（+1 个未识别会话），实际 %d 行: %+v", len(list), list)
	}
	var hit *qoderWebMeta
	for i := range list {
		if list[i].Email == "someone@example.com" {
			hit = &list[i]
		}
	}
	if hit == nil {
		t.Fatal("合并后仍应保留该账号")
	}
	if hit.Dir != "bbbb00000002" {
		t.Fatalf("应保留较新的那份会话，实际 %q", hit.Dir)
	}
	if len(hit.DupDirs) != 1 || hit.DupDirs[0] != "aaaa00000001" {
		t.Fatalf("旧副本应记进 DupDirs，实际 %v", hit.DupDirs)
	}
	if hit.LastCheckin == nil || hit.LastCheckin.Phase != "claimed" {
		t.Fatalf("签到状态应取较新的那份，实际 %+v", hit.LastCheckin)
	}

	// 保存新会话时把旧副本物理清掉，磁盘上不再留两份。
	removed := pruneQoderWebDuplicates(root, hit.Dir, *hit)
	if len(removed) != 1 || removed[0] != "aaaa00000001" {
		t.Fatalf("应清掉旧副本，实际 %v", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "aaaa00000001")); !os.IsNotExist(err) {
		t.Fatal("旧副本目录应已被删除")
	}
	if _, err := os.Stat(filepath.Join(root, "cccc00000003")); err != nil {
		t.Fatal("其它账号的会话不能被连带删除")
	}
}

// TestQoderWebDirNameGuard 删除接口只接受 shortHash 形态的目录名。
func TestQoderWebDirNameGuard(t *testing.T) {
	for _, ok := range []string{"6e2fa9f98dc2", "afb9d1dcf212", "0000000000ff"} {
		if !qoderWebDirNameOK(ok) {
			t.Errorf("合法目录名被拒: %q", ok)
		}
	}
	for _, bad := range []string{"", "..", "../etc", "6E2FA9F98DC2", "6e2fa9f98dc", "6e2fa9f98dc23", "6e2fa9f98dcz"} {
		if qoderWebDirNameOK(bad) {
			t.Errorf("非法目录名被接受: %q", bad)
		}
	}
}
