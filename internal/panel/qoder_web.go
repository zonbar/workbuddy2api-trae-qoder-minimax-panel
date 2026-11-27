// qoder_web.go Qoder「网页登录并签到」——每个号一个独立浏览器 profile。
//
// 为什么保留网页通道（2026-11-02 实测修正）：
// 官方活动页写明「Claim 100 Credits Daily，**只能在 Qoder 桌面客户端领取**」，
// 但**网页控制台（qoder.com/account/usage）同样能领**——它的成长页
// （openapi.qoder.sh/growth-page/activity-iframe）就是同一个领取入口。
//
// ⚠️ 但网页**不是**另一份额度：把成长页的前端 JS 拉下来看，它用的接口与客户端
// **完全一样**（GET /sash/api/v1/me/campaigns、POST /sash/api/v1/me/campaigns/{id}/claim、
// GET /sash/api/v1/me/campaigns/{id}/reward），认证方式不同（Cookie vs Bearer token），
// 身份也一样（同一个 uid）。所以：
//
//	· 换网页登录**不能**让一个号多领一份每日额度（额度按**人**发，见 qoder_person.go）；
//	· 但它仍有价值：① 客户端没装/没登录时也能签；② 每个号一份独立 profile，
//	  互不串登录态；③ 会话过期时可以用同一 profile 再起一次浏览器续上。
//
// 流程：
//  1. 每号一个 profile 目录 data/qoder-web/<账号哈希>/，启动浏览器打开
//     qoder.com/account/usage，你在这个窗口里登录一次（会话留在 profile 里，
//     以后不用再登）；
//  2. 面板用**浏览器级** CDP 抓全部 Cookie，用同一个 CookieJar 复现网页端
//     自己的请求：GET /sash/api/v1/me/campaigns → 有可领的 CLAIM_BENEFIT 就领；
//  3. 会话 Cookie 一并存进号池账号，之后每天后台直接用（过期了就用同一 profile
//     再起一次浏览器抓新的）。
package panel

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderWebOrigin Qoder 网页控制台。
const qoderWebOrigin = "https://qoder.com"

// qoderWebAPI Qoder 开放接口（成长页就是它）。国际站与国内站不是同一套域名，
// 支付宝扫码登录属于国内站，会话 cookie 只能打国内 openapi。
const (
	qoderWebAPI   = "https://openapi.qoder.sh"
	qoderWebAPICN = "https://openapi.qoder.com.cn"
)

// qoderSSOAlipayURL 构造国内版 Qoder 的阿里云 SSO 登录页。
// 参数与网页端自己拼的完全一致：client_id / redirect_uri / return_url 固定，
// nonce / state 每次随机，expireTime 取当前时间 +10 分钟。这样扫码登录不必
// 依赖从浏览器里抄来的一次性链接，在容器里也能随时重新发起。
func qoderSSOAlipayURL() string {
	q := url.Values{}
	q.Set("client_id", "qoder")
	q.Set("expireTime", strconv.FormatInt(time.Now().Add(10*time.Minute).UnixMilli(), 10))
	q.Set("nonce", randomHex(32))
	q.Set("redirect_uri", "https://qoder.com.cn/sso/callback/aliyun")
	q.Set("response_type", "code")
	q.Set("return_url", "https://qoder.com.cn")
	q.Set("scope", "openid profile")
	q.Set("state", randomHex(32))
	return "https://account.aliyun.com/sso/login.htm?" + q.Encode()
}

const qoderWebTTL = 30 * time.Minute

// qoderSSOSessionKey 未填邮箱时的会话键。
//
// 支付宝扫码走阿里云 SSO，扫码前根本不知道会登进哪个号，强制先填邮箱等于
// 让用户在「我还没登录」的时候回答「你登录的是谁」—— 多余的卡点。未填时
// 统一用这个键：一次只跑一个 SSO 会话，profile 目录也随之固定。
const qoderSSOSessionKey = "sso"

// qoderWebKey 会话键：优先显式邮箱，SSO 场景缺省回落固定键。
func qoderWebKey(email string, sso bool) string {
	if e := strings.TrimSpace(email); e != "" {
		return e
	}
	if sso {
		return qoderSSOSessionKey
	}
	return ""
}

// qoderWebSession 一次「网页登录 + 领取」的进行态（按账号分别记）。
var qoderWebSessions = struct {
	mu sync.Mutex
	by map[string]*qoderWebState
}{by: map[string]*qoderWebState{}}

type qoderWebState struct {
	email     string
	port      int
	dir       string
	proc      *exec.Cmd
	startedAt time.Time
	done      bool
	ok        bool
	message   string
	sso       bool // 支付宝扫码走国内站；普通网页登录走国际站
	// unauth 连续「已到应用页但接口 401」的次数。会话 Cookie 刚写入时接口
	// 可能还不认，一次 401 就判死会把「其实已经登录成功」的号误杀。
	unauth int
}

func (st *qoderWebState) apiBase() string {
	if st != nil && st.sso {
		return qoderWebAPICN
	}
	return qoderWebAPI
}

// qoderWebProfileDir 每号一个浏览器 profile（会话持久化 → 登录一次就够）。
func qoderWebProfileDir(p *Panel, email string) string {
	base := filepath.Join(traeBrowserBaseDir(p), "qoder-web")
	dir := filepath.Join(base, shortHash(email))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// shortHash 取邮箱的短哈希（做目录名，避免邮箱里的 @ 和 . ）。
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	h := hex.EncodeToString(sum[:])
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// qoderWebStart 拉起浏览器（该账号的独立 profile）并打开 Qoder 网页控制台。
func (p *Panel) qoderWebStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		SSO   bool   `json:"sso"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	email := qoderWebKey(req.Email, req.SSO)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}

	qoderWebSessions.mu.Lock()
	st := qoderWebSessions.by[email]
	if st != nil && st.proc != nil && !st.done {
		// 复用只对「刚拉起、进程还活着」的会话成立。用户再点一次扫码，多半是
		// 因为上一次的浏览器已经跳到登录后的页面（二维码没了）或者进程已退出，
		// 这时必须重新拉一次回到扫码页 —— 否则前端会一直「等待渲染二维码」，
		// 表现就是「第一次能扫，第二次拿不到图」。
		if time.Since(st.startedAt) < 90*time.Second && qoderWebProcAlive(st.proc) && qoderWebPortAlive(st.port) {
			qoderWebSessions.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true, "message": "浏览器已在运行，请用支付宝扫码登录"})
			return
		}
		qoderWebClose(st)
		log.Printf("panel: Qoder 重新拉起登录浏览器 %s（上次会话已超时或浏览器已退出）", email)
	}
	chrome, err := findChrome()
	if err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// profile 目录一律沿用历史路径（qoderWebProfileDir）：这是网页会话的存放位置，
	// 换成 egress 里的 profile_dir 会让已有登录态"消失"。契约 C-QD-EGRESS@1 要求
	// ProfileDir 与 qoderWebProfileDir 一致 —— 这里以它为准。
	dir := qoderWebProfileDir(p, email)
	egress := ideauth.QoderEgressFor(email)
	if pd := strings.TrimSpace(egress.ProfileDir); pd != "" && !strings.EqualFold(filepath.Clean(pd), filepath.Clean(dir)) {
		log.Printf("panel: Qoder 出口配置的 profile_dir=%s 与运行路径 %s 不一致，按运行路径启动", pd, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "创建浏览器 profile 目录失败: "+err.Error())
		return
	}
	// 这个 profile 是按邮箱固定复用的：上一次浏览器被 Kill 后留下的 SingletonLock
	// 会让新实例「转给不存在的旧实例然后静默退出」，表现为 CDP 端口一直不监听、
	// 前端永远「等待浏览器就绪…」。启动前清掉失效锁。
	if clearStaleChromiumSingleton(dir) {
		log.Printf("panel: Qoder 清理残留的浏览器单例锁 %s", filepath.Base(dir))
	}
	// 扫码登录走国内版阿里云 SSO（支付宝扫码入口）；普通网页登录仍走国际站。
	loginURL := qoderWebOrigin + "/account/usage"
	if req.SSO {
		loginURL = qoderSSOAlipayURL()
	}
	// 配置了出口代理的账号：登录环境与签到请求走同一个代理（否则"隔离"只隔离了一半）。
	args := headlessBrowserArgs(ideauth.QoderWebChromeArgs(port, dir, egress.ProxyURL, loginURL))
	cmd := exec.Command(chrome, args...)
	if err := cmd.Start(); err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	qoderWebSessions.by[email] = &qoderWebState{
		email: email, port: port, dir: dir, proc: cmd, startedAt: time.Now(), sso: req.SSO,
	}
	qoderWebSessions.mu.Unlock()
	// 端口是复用的：新浏览器必须重新点一次「支付宝」入口，否则会继承上一次
	// 会话的「已准备」标记，页面停在 SSO 首页不出二维码。
	browserQRForget(port, "qoder")
	log.Printf("panel: Qoder 网页登录：%s（调试端口 %d，profile %s，代理 %s）",
		email, port, dir, firstNonEmpty(egress.ProxyURL, "进程默认"))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true,
		"message": "已打开浏览器（这个账号的独立会话），请登录要添加的 Qoder 账号 —— 登录一次即可，以后每天由面板自动签到",
	})
}

// qoderWebPoll 轮询网页登录态；登录好了就把今天的每日额度领掉并存进号池。
func (p *Panel) qoderWebPoll(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		// 没带邮箱：SSO 扫码会话就是这个固定键，前端不必知道它。
		email = qoderSSOSessionKey
	}
	qoderWebSessions.mu.Lock()
	st := qoderWebSessions.by[email]
	qoderWebSessions.mu.Unlock()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "还没开始，请点「扫码登录（支付宝）」"})
		return
	}
	qoderWebSessions.mu.Lock()
	if st.done {
		res := map[string]any{"done": true, "ok": st.ok, "message": st.message}
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
		return
	}
	if time.Since(st.startedAt) > qoderWebTTL {
		st.done = true
		st.ok = false
		st.message = "等待登录超时，请重新开始"
		qoderWebClose(st)
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": st.message})
		return
	}
	port := st.port
	dir := st.dir
	apiBase := st.apiBase()
	qoderWebSessions.mu.Unlock()

	// 扫码成功后阿里云常常还要在页面上点一次「确认登录 / 授权」。容器里的浏览器
	// 没有窗口可点，这一步不代劳的话，登录就永远差最后一下。
	if clicked := qoderAutoConfirm(port); clicked != "" {
		log.Printf("panel: Qoder 自动确认授权 %q", clicked)
	}

	all, err := cdpAllCookies(port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待浏览器就绪…"})
		return
	}
	jar, cookies := qoderCookieJar(all)
	if len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Qoder 登录…"})
		return
	}

	// 用网页会话复现网页端自己的请求：先查活动列表。
	status, body, err := qoderWebGet(apiBase, jar, "/sash/api/v1/me/campaigns")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待浏览器就绪…"})
		return
	}
	// jar 严格按标准分发 Cookie：host-only 的会话 Cookie 不会被发给 openapi
	// 子域，但浏览器自己的 JS 请求会带上。401 时用原始 Cookie 头再试一次。
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if hdr := qoderRawCookieHeader(all, apiBase); hdr != "" {
			if s2, b2, e2 := qoderWebGetRaw(apiBase, hdr, "/sash/api/v1/me/campaigns"); e2 == nil && s2 == http.StatusOK {
				status, body = s2, b2
			}
		}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 判定前先看浏览器停在哪：还在支付宝 / 阿里云 / qoder 登录页上时，
		// qoder 域的那点 Cookie 只是访客态，401 说明「还没登完」，不是没权限。
		// 以前这里一次 401 就判死并关掉浏览器，正是「扫码后立刻报授权失败」的原因。
		if onApp, pageURL := qoderBrowserOnApp(port); !onApp {
			log.Printf("panel: Qoder 尚未完成登录（当前页 %s，HTTP %d），继续等待", firstNonEmpty(pageURL, "未知"), status)
			writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Qoder 登录…"})
			return
		}
		qoderWebSessions.mu.Lock()
		st.unauth++
		n := st.unauth
		qoderWebSessions.mu.Unlock()
		// 会话 Cookie 刚写入、接口还没认的情况很常见，给几轮缓冲。
		if n < 5 {
			writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "已登录，等待会话生效…"})
			return
		}
		qoderWebSessions.mu.Lock()
		st.done = true
		st.ok = false
		st.message = "登录后接口仍返回 HTTP " + fmt.Sprint(status) + "（已重试 " + fmt.Sprint(n) + " 次）。请重新点一次扫码登录，用要添加的那个 Qoder 账号扫码。"
		qoderWebClose(st)
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": false, "message": st.message})
		return
	}
	if status != http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": fmt.Sprintf("查询活动失败 HTTP %d", status)})
		return
	}

	var camps struct {
		Campaigns []struct {
			CampaignID  string `json:"campaignId"`
			ActionType  string `json:"actionType"`
			ClaimStatus string `json:"claimStatus"`
			Benefit     struct {
				Amount float64 `json:"amount"`
			} `json:"benefit"`
		} `json:"campaigns"`
	}
	if err := json.Unmarshal(body, &camps); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "活动列表解析失败"})
		return
	}

	claimID := ""
	amount := 0.0
	already := false
	for _, c := range camps.Campaigns {
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if c.ClaimStatus == "CLAIMABLE" {
			claimID = c.CampaignID
			amount = c.Benefit.Amount
			break
		}
		if c.ClaimStatus == "CLAIMED" {
			already = true
			amount = c.Benefit.Amount
		}
	}

	msg := ""
	ok := false
	switch {
	case claimID != "":
		st2, b2, cerr := qoderWebPost(apiBase, jar, "/sash/api/v1/me/campaigns/"+claimID+"/claim")
		if cerr == nil && st2 == http.StatusOK {
			var r struct {
				Status      string `json:"status"`
				FailureCode string `json:"failureCode"`
			}
			_ = json.Unmarshal(b2, &r)
			if r.Status == "CLAIMED" || r.Status == "SUCCESS" {
				ok = true
				msg = fmt.Sprintf("签到成功 +%.0f Credits（网页会话领取）", amount)
			} else {
				msg = "领取被服务端拒绝：" + r.FailureCode
			}
		} else {
			msg = fmt.Sprintf("领取失败 HTTP %d", st2)
		}
	case already:
		ok = true
		msg = fmt.Sprintf("今天已领取（+%.0f Credits）", amount)
	default:
		msg = "这个号今天服务端没有下发可领的每日额度（可能是同一个人的另一个账号已经领走了那一份）"
	}

	// 会话存进该账号的 profile 目录（cookies.json）：之后每天后台直接用，
	// 过期了再用同一 profile 起一次浏览器抓新的。
	if ok {
		if err := saveQoderWebCookies(dir, all); err != nil {
			log.Printf("panel: Qoder 网页会话落盘失败 %s: %v", email, err)
		}
	}
	// 浏览器用完就关：留着窗口既占内存又让用户以为"还有事没做完"。
	qoderWebClose(st)
	qoderWebSessions.mu.Lock()
	st.done = true
	st.ok = ok
	st.message = msg
	qoderWebSessions.mu.Unlock()
	log.Printf("panel: Qoder 网页登录 %s -> %s", email, msg)
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": ok, "message": msg, "email": email})
}

// qoderWebClose 关掉这次登录用的浏览器进程（幂等）。
//
// 为什么必须有：网页登录是「起一个浏览器窗口让人登录」的交互，一次流程结束后
// 浏览器还开着 —— 一个个窗口攒起来既占内存，也会让人以为流程没结束。
func qoderWebClose(st *qoderWebState) {
	if st == nil || st.proc == nil {
		return
	}
	p := st.proc
	st.proc = nil
	if p.Process == nil {
		return
	}
	_ = p.Process.Kill()
	_, _ = p.Process.Wait()
}

// qoderWebProcAlive 判断浏览器进程是否还在跑。
//
// exec.Cmd 只能告诉你「启动成功」，进程随后崩溃/退出它不会自己变；而面板把
// 「有 proc 且未 done」当成「浏览器在运行」，于是进程死了以后：扫码接口还返回
// 旧端口（截图必然失败）、再次点扫码也复用这个死会话 —— 用户看到的就是
// 「第一次能出图，第二次拿不到」。
func qoderWebProcAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

// qoderWebPortAlive 判断调试端口是否真的在监听。
//
// 只看进程是不够的：浏览器崩溃后没人 Wait，进程会变成僵尸，Signal(0) 依然
// 返回成功；而端口早已不监听 —— 于是「点扫码 → 说已在运行 → 截图 502」。
func qoderWebPortAlive(port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// qoderAutoConfirmJS 扫码后自动走完阿里云的授权确认。
//
// 容器里的浏览器没有窗口可点，支付宝扫码成功后如果停在「确认登录 / 授权」
// 页面，登录就永远差最后一步：qoder.com.cn 拿不到会话，接口一路 401，
// 用户只能看到「这个浏览器会话拿不到 openapi 的授权」。这里只在阿里云域上
// 点语义明确的确认类按钮，不做任何输入类操作。
const qoderAutoConfirmJS = `(() => {
  const host = location.hostname || '';
  if (host.indexOf('aliyun') < 0) return JSON.stringify('');
  const texts = ['确认登录', '同意并登录', '确认授权', '同意授权', '授权登录', '继续登录', '确认', '同意'];
  for (const el of document.querySelectorAll('button,a,[role="button"],input[type="submit"],span[class*="btn" i]')) {
    const t = ((el.textContent || el.value || '')).trim();
    if (!t || t.length > 10) continue;
    if (texts.indexOf(t) < 0) continue;
    try { el.click(); return JSON.stringify(t); } catch (e) {}
  }
  return JSON.stringify('');
})()`

// qoderAutoConfirm 在当前页面上尝试点一次授权确认按钮，返回点到的文案。
func qoderAutoConfirm(port int) string {
	pages, err := caListPages(port)
	if err != nil {
		return ""
	}
	for _, pg := range pages {
		u, err := url.Parse(pg.URL)
		if err != nil || !strings.Contains(strings.ToLower(u.Host), "aliyun") {
			continue
		}
		ws, err := wsDial(pg.WS, 6*time.Second)
		if err != nil {
			continue
		}
		raw, err := caEval(ws, qoderAutoConfirmJS)
		ws.Close()
		if err != nil {
			continue
		}
		var clicked string
		if json.Unmarshal(raw, &clicked) == nil && clicked != "" {
			return clicked
		}
	}
	return ""
}

// qoderBrowserOnApp 判断浏览器是否已经停在 Qoder 的应用页面（登录页 / SSO 之外）。
//
// 这是 401 的唯一可信上下文：还在支付宝或阿里云页面上时，qoder 域里的那点
// Cookie 只是访客态的，接口当然 401 —— 那不是「没权限」，是「还没登完」。
func qoderBrowserOnApp(port int) (bool, string) {
	pages, err := caListPages(port)
	if err != nil {
		return false, ""
	}
	for _, pg := range pages {
		u, err := url.Parse(pg.URL)
		if err != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(u.Host), "qoder") {
			continue
		}
		p := strings.ToLower(u.Path)
		if strings.Contains(p, "login") || strings.Contains(p, "signin") || strings.Contains(p, "sso") {
			continue
		}
		return true, pg.URL
	}
	return false, ""
}

// qoderRawCookieHeader 把 qoder 域下的 Cookie 直接拼成 Cookie 头。
//
// 按标准，host-only Cookie 只发给设置它的那个主机；但网页自己的 JS 请求
// openapi 子域时浏览器是按站点规则带上它们的。Go 的 cookiejar 严格实现标准，
// 遇到 host-only 会话就会漏带，于是「浏览器里明明登录了，接口却 401」。
// 这里不做任何过滤，凡是域与目标主机同一站点的全带上，作为 401 的兜底。
func qoderRawCookieHeader(all []cdpCookie, target string) string {
	tu, err := url.Parse(target)
	if err != nil {
		return ""
	}
	host := strings.ToLower(tu.Host)
	var parts []string
	seen := map[string]bool{}
	for _, c := range all {
		if c.Name == "" || c.Value == "" {
			continue
		}
		d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if d == "" || !strings.Contains(d, "qoder") {
			continue
		}
		if host != d && !strings.HasSuffix(host, "."+d) {
			continue
		}
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// qoderWebGetRaw 用显式 Cookie 头发一次 GET（不走 cookiejar）。
func qoderWebGetRaw(base, cookieHeader, path string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// saveQoderWebCookies 把网页会话 Cookie 落到该账号的 profile 目录（0600）。
func saveQoderWebCookies(dir string, all []cdpCookie) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var keep []cdpCookie
	for _, c := range all {
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if !strings.Contains(host, "qoder") || c.Name == "" || c.Value == "" {
			continue
		}
		keep = append(keep, c)
	}
	buf, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cookies.json"), buf, 0o600)
}

// loadQoderWebCookies 读回该账号的网页会话 Cookie。
func loadQoderWebCookies(dir string) []cdpCookie {
	raw, err := os.ReadFile(filepath.Join(dir, "cookies.json"))
	if err != nil {
		return nil
	}
	var out []cdpCookie
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// qoderCookieJar 把抓到的 Cookie 装进 jar（按域规则），并返回登录态域名列表。
func qoderCookieJar(all []cdpCookie) (*cookiejar.Jar, []string) {
	jar, _ := cookiejar.New(nil)
	var doms []string
	seen := map[string]bool{}
	for _, c := range all {
		if c.Name == "" || c.Value == "" {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if !strings.Contains(host, "qoder") {
			continue
		}
		if !seen[host] {
			seen[host] = true
			doms = append(doms, host)
		}
		u := &url.URL{Scheme: "https", Host: host, Path: c.Path}
		if u.Path == "" {
			u.Path = "/"
		}
		jar.SetCookies(u, []*http.Cookie{{
			Name: c.Name, Value: c.Value, Path: u.Path, Domain: c.Domain,
			HttpOnly: c.HTTPOnly, Secure: c.Secure,
		}})
	}
	return jar, doms
}

// qoderWebGet / qoderWebPost 用网页会话调 openapi（CookieJar 自动按域挑 cookie）。
func qoderWebGet(base string, jar *cookiejar.Jar, path string) (int, []byte, error) {
	return qoderWebDo(base, jar, http.MethodGet, path, nil)
}

func qoderWebPost(base string, jar *cookiejar.Jar, path string) (int, []byte, error) {
	return qoderWebDo(base, jar, http.MethodPost, path, []byte("{}"))
}

func qoderWebDo(base string, jar *cookiejar.Jar, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var rdr io.Reader
	if len(body) > 0 {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
