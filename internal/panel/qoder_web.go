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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderWebOrigin Qoder 网页控制台。
const qoderWebOrigin = "https://qoder.com"

// qoderWebOriginCN 国内站控制台（支付宝扫码登录落在这里）。
const qoderWebOriginCN = "https://qoder.com.cn"

// qoderWebAPI / qoderWebAPICN 网页会话调接口的基地址 —— **站点自己**，不是 openapi。
//
// ⚠️ 这里以前写的是 openapi.qoder.sh / openapi.qoder.com.cn，是「扫码成功后
// 接口永远 401」的根因。实测（2026-11-27，容器内带真实会话）：
//
//	GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns
//	→ 401 {"code":"TOKEN_INVALID","message":"missing authorization token"}
//
//	GET https://qoder.com.cn/sash/api/v1/me/campaigns（带 qoder_session_cookie）
//	→ 200 {"uid":"01a2...","campaigns":[...]}
//
// 两套接口路径完全一样，但鉴权方式不同：openapi 只认
// `Authorization: Bearer <token>`（客户端那条链路），网页会话的 Cookie 它一概
// 不认 —— 所以浏览器里明明登录成功（profile 里有 qoder_session_cookie），
// 面板打接口却一路 401，最后还要用户「重新扫码」。
//
// 网页端自己也不直连 openapi：成长页 iframe 的 fetch 甚至写着 credentials:"omit"，
// 请求由 qoder.com.cn 的同源代理转发，代理才读 Cookie（无 Cookie 时报
// "missing cookie header"）。因此网页会话必须打站点起源，路径仍是 /sash/api/...
const (
	qoderWebAPI   = qoderWebOrigin
	qoderWebAPICN = qoderWebOriginCN
)

// qoderWebMePath 站点同源代理下的账号信息接口。
//
// /sash/api/v1/me 在代理下是 404；真正带身份的是站点自己的 /api/v1/me
// （返回 id / name / email）。身份识别用它，别再用 /sash 那条路径。
const qoderWebMePath = "/api/v1/me"

// qoderWebSignInURL 国内版 Qoder 自己的登录页。
//
// ⚠️ 不要自己拼阿里云 SSO 链接。实测（2026-11-27）手工拼
// account.aliyun.com/sso/login.htm?client_id=qoder&redirect_uri=... 时，
// 阿里云照样能出二维码、扫码也回调带回了 code，但
// https://qoder.com.cn/sso/callback/aliyun 直接吐
// "Bad Request — Sorry, your request is invalid"，一个会话 Cookie 都拿不到。
//
// 原因是这条链路的前半段状态由 qoder 前端自己保管（nonce/state 与前端
// 存储绑定，回调用它换会话）。从阿里云页面「空降」进去，回调时对不上，
// 服务端就判请求非法。
//
// 正确做法是回到站点自己的登录页，让页面自己发起 SSO：
//
//	/users/sign-in → 点「使用阿里云登录」→ 阿里云 SSO → 点「支付宝」→ 扫码
//
// 这样整条链的参数与状态都是站点自己写的，回调能正常换到会话。
const qoderWebSignInURL = "https://qoder.com.cn/users/sign-in"

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
	// throttleUntil / throttleN 上游 429 的退避窗口与连续次数。
	//
	// 前端 2.5 秒一轮，每轮都打 /me/campaigns —— 未登录时是 401，登录后是
	// 真业务请求，连续几十次必然被限流（实测用户看到「查询活动失败 HTTP 429」）。
	// 一旦 429，按 10s→20s→40s（上限 60s）退避，退避期内直接返回等待文案，
	// 不再把请求打到上游 —— 越打越限，只会把「已经登录成功」的会话拖死。
	throttleUntil time.Time
	throttleN     int
}

func (st *qoderWebState) apiBase() string {
	if st != nil && st.sso {
		return qoderWebAPICN
	}
	return qoderWebAPI
}

// qoderWebProfileDir 每号一个浏览器 profile（会话持久化 → 登录一次就够）。
// qoderWebProfileRoot 网页会话 profile 的根目录（每个号一个子目录）。
func qoderWebProfileRoot(p *Panel) string {
	base := filepath.Join(traeBrowserBaseDir(p), "qoder-web")
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	return base
}

func qoderWebProfileDir(p *Panel, email string) string {
	dir := filepath.Join(qoderWebProfileRoot(p), shortHash(email))
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
		loginURL = qoderWebSignInURL
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
	msg := "已打开浏览器（这个账号的独立会话），请登录要添加的 Qoder 账号 —— 登录一次即可，以后每天由面板自动签到"
	if req.SSO {
		// 扫码这条路是面板代点：登录页 → 使用阿里云登录 → 支付宝，用户什么都不用操作。
		msg = "已打开 Qoder 登录页，正在准备支付宝二维码…"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true,
		"message": msg,
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

	// 回调页明确报错时立刻收尾：等下去不会有会话，只会耗到 30 分钟超时。
	if broken, cur := qoderWebCallbackBroken(port); broken {
		qoderWebSessions.mu.Lock()
		st.done = true
		st.ok = false
		st.message = "登录回调被 Qoder 拒绝（回调页提示 Bad Request）。这次登录链接已失效，请重新点一次「扫码登录（支付宝）」，用要添加的账号重新扫。"
		qoderWebSessions.mu.Unlock()
		qoderWebClose(st)
		log.Printf("panel: Qoder 回调页报错，本次登录作废（%s）", cur)
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": false, "message": st.message})
		return
	}

	all, err := cdpAllCookies(port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待浏览器就绪…"})
		return
	}
	jar, cookies := qoderCookieJar(all)
	// 只在「看起来已经登录」时才打业务接口。
	//
	// 之前只要有任意 qoder 域 Cookie 就打 /me/campaigns —— 而访客 Cookie
	// （cna / tfstk / acw_tc / arms_uid ...）一进站就有，于是扫码还没完成就
	// 每 2.5 秒打一次：未登录时全是 401，登录后又把接口打到 429。
	// 用户看到的就是一串「查询活动失败 HTTP 429」+「等待在浏览器中完成登录」。
	onApp, pageURL := qoderBrowserOnApp(port)
	if len(cookies) == 0 || (!onApp && !qoderWebHasSessionCookie(all)) {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Qoder 登录…"})
		return
	}

	// 429 退避窗口：越打越限，退避期内不再发请求。
	qoderWebSessions.mu.Lock()
	wait := time.Until(st.throttleUntil)
	qoderWebSessions.mu.Unlock()
	if wait > 0 {
		secs := int(wait.Seconds()) + 1
		writeJSON(w, http.StatusOK, map[string]any{
			"done":    false,
			"message": fmt.Sprintf("服务端限流，%d 秒后自动重试…", secs),
		})
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
	if status == http.StatusTooManyRequests {
		qoderWebSessions.mu.Lock()
		st.throttleN++
		backoff := 10 * time.Second << uint(min(st.throttleN-1, 3)) // 10s / 20s / 40s / 80s
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
		st.throttleUntil = time.Now().Add(backoff)
		n := st.throttleN
		qoderWebSessions.mu.Unlock()
		log.Printf("panel: Qoder 活动接口返回 429，第 %d 次退避 %s", n, backoff)
		writeJSON(w, http.StatusOK, map[string]any{
			"done":    false,
			"message": fmt.Sprintf("服务端限流（HTTP 429），%d 秒后自动重试…", int(backoff.Seconds())),
		})
		return
	}
	if status == http.StatusOK {
		// 能拿到活动列表就说明会话是有效的，退避计数清零。
		qoderWebSessions.mu.Lock()
		st.throttleN = 0
		st.throttleUntil = time.Time{}
		qoderWebSessions.mu.Unlock()
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 判定前先看浏览器停在哪：还在支付宝 / 阿里云 / qoder 登录页上时，
		// qoder 域的那点 Cookie 只是访客态，401 说明「还没登完」，不是没权限。
		// 以前这里一次 401 就判死并关掉浏览器，正是「扫码后立刻报授权失败」的原因。
		if !onApp {
			log.Printf("panel: Qoder 尚未完成登录（当前页 %s，HTTP %d），继续等待", firstNonEmpty(pageURL, "未知"), status)
			writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Qoder 登录…"})
			return
		}
		qoderWebSessions.mu.Lock()
		st.unauth++
		n := st.unauth
		qoderWebSessions.mu.Unlock()
		// 401 的现场只有这一次能抓到（浏览器随后就被关掉），把 Cookie 名字与
		// 响应体留在日志里，下次再出问题不必让用户重扫一遍才知道差什么。
		log.Printf("panel: Qoder 活动接口 HTTP %d（当前页 %s，cookie=%v）：%s",
			status, firstNonEmpty(pageURL, "未知"), qoderCookieNames(all), qoderBodySnippet(body))
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
		// 200 就说明会话是好的、账号已经拿到 —— 只是今天这个号没有可领的额度。
		// 这里以前把 ok 留成 false，前端就把「账号已添加」渲染成红字失败，
		// 用户看到的是「账号仍然没添加成功」，其实早就加进去了。
		ok = true
		msg = "登录成功，账号已添加；今天服务端没有可领的每日额度（可能已经领过，或同一人的另一个号已领走）"
	}

	// 把这次领取的结果也记成签到状态：签到卡上能立刻看到今天这个号的情况，
	// 不用等下一轮自动签到才有数据。
	chk := qoderWebCheckinResult{At: time.Now().Format(time.RFC3339), Msg: msg, Amount: amount}
	switch {
	case claimID != "" && ok:
		chk.Phase = "claimed"
	case claimID != "":
		chk.Phase = "error"
	case already:
		chk.Phase = "already"
	default:
		chk.Phase = "pending"
	}

	// 会话存进该账号的 profile 目录（cookies.json）：之后每天后台直接用，
	// 过期了再用同一 profile 起一次浏览器抓新的。
	//
	// ⚠️ 这里必须**无条件**保存：之前的条件是「领到了才存」，于是「今天已领过」
	// 「服务端今天没下发额度」这些正常情况全都不落盘 —— 用户明明登录成功，
	// 面板里却什么都没有，表现就是「账号还是没添加成功」。
	// 能走到这里就说明 campaigns 返回了 200，会话本身是有效的，值得保存。
	if err := saveQoderWebCookies(dir, all); err != nil {
		log.Printf("panel: Qoder 网页会话落盘失败 %s: %v", email, err)
	}
	// 记下账号身份：号池接口与自动签到都靠它把「这份会话属于谁」认出来。
	idEmail, idName, idUID := qoderWebIdentity(port, apiBase, jar, all)
	if idEmail != "" && email == qoderSSOSessionKey {
		email = idEmail
	}
	meta := qoderWebMeta{
		Email: idEmail, Name: idName, UID: idUID,
		SessionKey: email, APICN: st.sso, SavedAt: time.Now().Format(time.RFC3339),
		LastCheckin: &chk,
	}
	// meta.json 是整体覆写的；LastCheckin 用本次登录的真实领取结果覆盖即可
	// （这次登录刚刚打过上游，比历史记录新）。
	if err := saveQoderWebMeta(dir, meta); err != nil {
		log.Printf("panel: Qoder 网页会话身份落盘失败 %s: %v", email, err)
	}
	// 同一个号换过会话键再登录一次，磁盘上就有两份 profile —— 清掉旧的，
	// 免得签到卡里同一个账号出现两行（用户会当成「重复添加」）。
	if removed := pruneQoderWebDuplicates(qoderWebProfileRoot(p), dir, meta); len(removed) > 0 {
		log.Printf("panel: Qoder 账号 %s 清理旧会话副本 %v", firstNonEmpty(meta.Email, meta.UID), removed)
	}
	// 浏览器用完就关：留着窗口既占内存又让用户以为"还有事没做完"。
	qoderWebClose(st)
	if !ok {
		// 没领到不代表失败：会话已保存，账号已记下，明天起自动签到能直接用它。
		msg += "；网页会话已保存，账号已记下，之后每日自动签到会用它"
	}
	qoderWebSessions.mu.Lock()
	st.done = true
	st.ok = ok
	st.message = msg
	qoderWebSessions.mu.Unlock()
	log.Printf("panel: Qoder 网页登录 %s -> %s", email, msg)
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": ok, "message": msg, "email": email, "name": idName, "user_id": idUID,
	})
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
	cur := ""
	for _, pg := range pages {
		u, err := url.Parse(pg.URL)
		if err != nil {
			continue
		}
		if cur == "" && strings.Contains(strings.ToLower(u.Host), "qoder") {
			cur = pg.URL
		}
		if !strings.Contains(strings.ToLower(u.Host), "qoder") {
			continue
		}
		p := strings.ToLower(u.Path)
		if strings.Contains(p, "login") || strings.Contains(p, "signin") || strings.Contains(p, "sign-in") || strings.Contains(p, "sso") {
			continue
		}
		return true, pg.URL
	}
	return false, cur
}

// qoderWebCurrentURL 返回浏览器当前停在哪（任意页面，用于日志与用户提示）。
//
// 之前只在「已进入应用页」时才返回 URL，其余情况一律空串，日志里就成了
// 「当前页 未知」—— 恰好把最有用的线索（卡在哪个页面）丢掉了。
func qoderWebCurrentURL(port int) string {
	pages, err := caListPages(port)
	if err != nil {
		return ""
	}
	for _, pg := range pages {
		if strings.Contains(strings.ToLower(pg.URL), "qoder") {
			return pg.URL
		}
	}
	if len(pages) > 0 {
		return pages[0].URL
	}
	return ""
}

// qoderWebCallbackBroken 判断 SSO 回调页是否明确报错。
//
// 实测（2026-11-27）手工拼 SSO 链接时，浏览器会停在
// https://qoder.com.cn/sso/callback/aliyun?code=... 并显示
// "Bad Request — Sorry, your request is invalid"。
// 这种情况下等多久都不会有会话：必须立刻告诉用户重来，
// 而不是让它耗到 30 分钟超时后报「等待登录超时」。
func qoderWebCallbackBroken(port int) (bool, string) {
	cur := qoderWebCurrentURL(port)
	if cur == "" || !strings.Contains(strings.ToLower(cur), "/sso/callback") {
		return false, cur
	}
	ws, _, err := caPickPage(port)
	if err != nil {
		return false, cur
	}
	defer ws.Close()
	raw, err := caEval(ws, qoderWebBodyTextJS)
	if err != nil {
		return false, cur
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return false, cur
	}
	low := strings.ToLower(text)
	broken := strings.Contains(low, "bad request") || strings.Contains(low, "request is invalid") ||
		strings.Contains(low, "invalid request")
	return broken, cur
}

const qoderWebBodyTextJS = `(document.body ? document.body.innerText : '').slice(0, 600)`

// qoderWebMeta 一份网页会话的账号身份（与 cookies.json 同目录）。
//
// 以前只存 cookies，谁也不知道这份会话属于哪个号：界面没法展示、自动签到也
// 无法按号遍历，于是「扫码登录成功」在用户眼里等于「什么也没发生」。
// qoderWebCheckinResult 一次网页会话签到的结果。
//
// 落进 meta.json，界面与日志共用同一份 —— 否则「今天这个号到底领到没有」
// 只能翻容器日志，用户在面板上永远看不到。
type qoderWebCheckinResult struct {
	Phase  string  `json:"phase"` // claimed / already / pending / expired / limited / error
	Amount float64 `json:"amount"`
	Msg    string  `json:"msg"`
	At     string  `json:"at"`
}

type qoderWebMeta struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	SessionKey string `json:"session_key"`
	APICN      bool   `json:"api_cn"`
	SavedAt    string `json:"saved_at"`
	// LastCheckin 最近一次签到的结果（自动签到与手动签到都写）。
	LastCheckin *qoderWebCheckinResult `json:"last_checkin,omitempty"`
	// Dir 是 profile 目录名（只在列出会话时填，不落盘）。
	Dir string `json:"-"`
	// DupDirs 是同一个账号的多余会话目录（只在列出会话时填，不落盘）。
	//
	// 同一个号被登录两次就有两份：先留空邮箱扫码（键 = sso），后来又填了邮箱
	// 另建一份（键 = 邮箱）。两份 cookie 都有效、身份也认得出，界面上就是
	// 「账号重复出现」—— 这里把它们折进主会话，界面上一个号只出现一次。
	DupDirs []string `json:"-"`
}

// qoderWebSessionIdent 会话对应的账号身份键。
//
// 只有认得出身份（邮箱或 uid）的会话才参与去重：两段都空说明还没登录成功，
// 按目录各自独立，不能当成同一个号。
func qoderWebSessionIdent(m qoderWebMeta) string {
	if e := strings.ToLower(strings.TrimSpace(m.Email)); e != "" {
		return "email:" + e
	}
	if u := strings.TrimSpace(m.UID); u != "" {
		return "uid:" + u
	}
	return ""
}

func qoderWebMetaPath(dir string) string { return filepath.Join(dir, "meta.json") }

// saveQoderWebMeta 原子写 meta.json（0600）。
func saveQoderWebMeta(dir string, m qoderWebMeta) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("empty dir")
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := qoderWebMetaPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, qoderWebMetaPath(dir))
}

// loadQoderWebMeta 读回 meta.json（不存在返回零值，不报错）。
func loadQoderWebMeta(dir string) qoderWebMeta {
	raw, err := os.ReadFile(qoderWebMetaPath(dir))
	if err != nil {
		return qoderWebMeta{}
	}
	var m qoderWebMeta
	if json.Unmarshal(raw, &m) != nil {
		return qoderWebMeta{}
	}
	return m
}

// readQoderWebSessions 原始列举：一个 profile 目录一条，不做合并。
func readQoderWebSessions(root string) []qoderWebMeta {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]qoderWebMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "cookies.json")); err != nil {
			continue
		}
		m := loadQoderWebMeta(dir)
		m.Dir = e.Name()
		if m.SessionKey == "" {
			m.SessionKey = e.Name()
		}
		out = append(out, m)
	}
	return out
}

// listQoderWebSessions 列出账号级会话：同一账号的多份 profile 合并成一条。
//
// 为什么要合并：扫码时可以留空邮箱（键 = sso），也可以填邮箱（键 = 邮箱），
// 同一个号用两种方式各登录一次，就会在 data/qoder-web 下留两个目录。
// 不合并的话界面上同一个号显示两行，用户会以为「重复添加了」。
func listQoderWebSessions(root string) []qoderWebMeta {
	all := readQoderWebSessions(root)
	best := make(map[string]int, len(all))
	for i := range all {
		key := qoderWebSessionIdent(all[i])
		if key == "" {
			continue
		}
		j, ok := best[key]
		if !ok {
			best[key] = i
			continue
		}
		win, lose := i, j
		if !qoderWebMetaNewer(all[i], all[j]) {
			win, lose = j, i
		}
		// 被合并的那份连同它自己的副本一起挂到胜者名下，_remove 时一并清理。
		all[win].DupDirs = append(all[win].DupDirs, all[lose].Dir)
		all[win].DupDirs = append(all[win].DupDirs, all[lose].DupDirs...)
		// 签到状态取更新的那份，否则「今天已领」会被旧副本的空记录盖掉。
		if qoderWebResultNewer(all[lose].LastCheckin, all[win].LastCheckin) {
			all[win].LastCheckin = all[lose].LastCheckin
		}
		best[key] = win
	}
	out := make([]qoderWebMeta, 0, len(all))
	for i := range all {
		if key := qoderWebSessionIdent(all[i]); key != "" && best[key] != i {
			continue
		}
		out = append(out, all[i])
	}
	return out
}

// qoderWebMetaNewer 判断两份会话哪份更新（SavedAt 为主，目录名兜底保证稳定）。
func qoderWebMetaNewer(a, b qoderWebMeta) bool {
	ta, ea := time.Parse(time.RFC3339, strings.TrimSpace(a.SavedAt))
	tb, eb := time.Parse(time.RFC3339, strings.TrimSpace(b.SavedAt))
	if ea == nil && eb == nil && !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.Dir > b.Dir
}

// qoderWebResultNewer 判断签到记录哪个更新（nil 视为最旧）。
func qoderWebResultNewer(a, b *qoderWebCheckinResult) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	ta, ea := time.Parse(time.RFC3339, a.At)
	tb, eb := time.Parse(time.RFC3339, b.At)
	if ea == nil && eb == nil {
		return ta.After(tb)
	}
	return a.At > b.At
}

// pruneQoderWebDuplicates 删掉同一账号的旧副本（保留刚登录成功的 keepDir）。
//
// 新会话刚拿到 cookie，旧目录里的那份必然更旧；留着只会让界面出现重复条目，
// 而且自动签到会拿旧 cookie 去换新 cookie，覆盖掉刚拿到的那份。
func pruneQoderWebDuplicates(root, keepDir string, m qoderWebMeta) []string {
	key := qoderWebSessionIdent(m)
	if key == "" {
		return nil
	}
	var removed []string
	for _, o := range readQoderWebSessions(root) {
		if o.Dir == keepDir || qoderWebSessionIdent(o) != key {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, o.Dir)); err != nil {
			log.Printf("panel: 清理 Qoder 重复会话 %s 失败: %v", o.Dir, err)
			continue
		}
		removed = append(removed, o.Dir)
	}
	return removed
}

// qoderWebIdentity 尽力识别这次登录的账号身份。
//
// 扫码登录走的是阿里云 SSO，扫码前根本不知道会登进谁，所以身份只能在登录
// 完成后反查：先问接口（最准），再退到页面存储里找邮箱/uid。
// 两者都拿不到时返回空值 —— 调用方仍会保存会话，只是界面标注「未识别」。
func qoderWebIdentity(port int, apiBase string, jar *cookiejar.Jar, all []cdpCookie) (email, name, uid string) {
	for _, p := range []string{qoderWebMePath, "/sash/api/v1/me"} {
		st, body, err := qoderWebGet(apiBase, jar, p)
		if err != nil || st != http.StatusOK {
			continue
		}
		var m map[string]any
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		e, n, u := pickQoderIdentity(m)
		if e != "" || u != "" {
			return e, n, u
		}
	}
	if hdr := qoderRawCookieHeader(all, apiBase); hdr != "" {
		for _, p := range []string{qoderWebMePath, "/sash/api/v1/me"} {
			st, body, err := qoderWebGetRaw(apiBase, hdr, p)
			if err != nil || st != http.StatusOK {
				continue
			}
			var m map[string]any
			if json.Unmarshal(body, &m) != nil {
				continue
			}
			e, n, u := pickQoderIdentity(m)
			if e != "" || u != "" {
				return e, n, u
			}
		}
	}
	// 退到页面：应用页一般把用户信息放在 localStorage / 全局对象里。
	ws, _, err := caPickPage(port)
	if err != nil {
		return "", "", ""
	}
	defer ws.Close()
	raw, err := caEval(ws, qoderWebIdentityJS)
	if err != nil {
		return "", "", ""
	}
	var got struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		UID   string `json:"uid"`
	}
	if json.Unmarshal(raw, &got) != nil {
		return "", "", ""
	}
	return got.Email, got.Name, got.UID
}

// pickQoderIdentity 从任意层级的 JSON 里找出 email / name / uid。
func pickQoderIdentity(m map[string]any) (email, name, uid string) {
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				lk := strings.ToLower(k)
				if s, ok := val.(string); ok {
					switch lk {
					case "email", "mail", "useremail", "loginemail":
						if email == "" && strings.Contains(s, "@") {
							email = s
						}
					case "name", "nickname", "displayname", "username":
						// 站点 /api/v1/me 的 name 有时就是邮箱（实测 zonbar@163.com），
						// email 字段反而是空的 —— 这种当邮箱收，别当昵称丢掉。
						// username 常常是内部 uuid（124b05dc-...），当昵称会很难看。
						if s == "" {
							continue
						}
						if strings.Contains(s, "@") {
							if email == "" {
								email = s
							}
						} else if lk == "username" {
							if name == "" && !looksLikeUUID(s) {
								name = s
							}
						} else if name == "" {
							name = s
						}
					case "uid", "userid", "id":
						if uid == "" && s != "" {
							uid = s
						}
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(m)
	return
}

// looksLikeUUID 判断一个字符串是不是 uuid（内部 id 不适合当昵称展示）。
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// qoderWebIdentityJS 在页面里找账号身份：先看常见全局对象，再扫 Web Storage。
const qoderWebIdentityJS = `(() => {
  const out = {email: '', name: '', uid: ''};
  const emailRe = /[A-Za-z0-9._%+-]+\@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/;
  const take = (obj) => {
    if (!obj || typeof obj !== 'object') return;
    for (const k of ['email', 'mail', 'userEmail', 'loginEmail']) {
      if (!out.email && typeof obj[k] === 'string' && emailRe.test(obj[k])) out.email = obj[k];
    }
    for (const k of ['name', 'nickName', 'nickname', 'displayName', 'userName']) {
      if (!out.name && typeof obj[k] === 'string' && obj[k] && !emailRe.test(obj[k])) out.name = obj[k];
    }
    for (const k of ['uid', 'userId', 'id']) {
      if (!out.uid && (typeof obj[k] === 'string' || typeof obj[k] === 'number') && String(obj[k])) out.uid = String(obj[k]);
    }
  };
  for (const g of ['__INITIAL_STATE__', '__NUXT__', '__NEXT_DATA__', 'userInfo', '__USER_INFO__']) {
    try { take(window[g]); } catch (e) {}
  }
  const scan = (store) => {
    try {
      for (let i = 0; i < store.length; i++) {
        const k = store.key(i), v = store.getItem(k) || '';
        if (!out.email) { const m = v.match(emailRe); if (m) out.email = m[0]; }
        if (/user|account|profile|auth/i.test(k)) {
          try { take(JSON.parse(v)); } catch (e) {}
        }
      }
    } catch (e) {}
  };
  scan(localStorage); scan(sessionStorage);
  return JSON.stringify(out);
})()`

// qoderWebCheckinSummary 一轮批量签到的计数。
type qoderWebCheckinSummary struct {
	Claimed int `json:"claimed"`
	Already int `json:"already"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
	Expired int `json:"expired"`
	Limited int `json:"limited"`
}

// qoderWebCheckinOne 给单个网页会话领一次每日额度，并把结果写回 meta.json。
//
// 网页会话和 IDE 号池是两条独立链路：前者手里只有 Cookie（打站点同源接口），
// 后者是 IDE 授权快照。所以签到不能共用代码，但结果形态刻意保持一致
// （claimed / already / pending / expired / error），界面才能用同一套徽章。
func qoderWebCheckinOne(root string, m qoderWebMeta) qoderWebCheckinResult {
	res := qoderWebCheckinResult{At: time.Now().Format(time.RFC3339)}
	label := firstNonEmpty(m.Email, m.SessionKey, m.Dir)
	// 不管成功、失败还是会话失效，结果都要落盘 —— 否则界面上这个号永远停在
	// 「未签到」，用户根本看不出是没签、签失败、还是要重新登录。
	defer func() {
		if res.Phase == "" {
			return
		}
		log.Printf("panel: Qoder 网页会话签到 %s → %s（%s）", label, res.Phase, res.Msg)
		m.LastCheckin = &res
		if err := saveQoderWebMeta(filepath.Join(root, m.Dir), m); err != nil {
			log.Printf("panel: Qoder 网页会话 %s 签到结果落盘失败: %v", label, err)
		}
	}()
	all := loadQoderWebCookies(filepath.Join(root, m.Dir))
	if len(all) == 0 {
		res.Phase, res.Msg = "expired", "会话里没有保存的 Cookie，需要重新扫码登录"
		return res
	}
	jar, _ := qoderCookieJar(all)
	apiBase := qoderWebAPI
	if m.APICN {
		apiBase = qoderWebAPICN
	}

	status, body, err := qoderWebGet(apiBase, jar, "/sash/api/v1/me/campaigns")
	if err != nil {
		res.Phase, res.Msg = "error", "请求上游失败："+err.Error()
		return res
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if hdr := qoderRawCookieHeader(all, apiBase); hdr != "" {
			if s2, b2, e2 := qoderWebGetRaw(apiBase, hdr, "/sash/api/v1/me/campaigns"); e2 == nil && s2 == http.StatusOK {
				status, body = s2, b2
			}
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Phase = "expired"
		res.Msg = "会话已失效（HTTP " + strconv.Itoa(status) + "），需要重新扫码登录"
		return res
	case status == http.StatusTooManyRequests:
		res.Phase, res.Msg = "limited", "上游限流（HTTP 429），下一轮再试"
		return res
	case status != http.StatusOK:
		res.Phase, res.Msg = "error", "上游返回 HTTP "+strconv.Itoa(status)
		return res
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
		res.Phase, res.Msg = "error", "活动列表解析失败："+err.Error()
		return res
	}
	claimID, amount, isAlready := "", 0.0, false
	for _, c := range camps.Campaigns {
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if c.ClaimStatus == "CLAIMABLE" {
			claimID, amount = c.CampaignID, c.Benefit.Amount
			break
		}
		if c.ClaimStatus == "CLAIMED" {
			isAlready, amount = true, c.Benefit.Amount
		}
	}
	switch {
	case claimID != "":
		s2, b2, e2 := qoderWebPost(apiBase, jar, "/sash/api/v1/me/campaigns/"+claimID+"/claim")
		if e2 != nil || s2 != http.StatusOK {
			res.Phase = "error"
			res.Msg = "领取接口返回 HTTP " + strconv.Itoa(s2)
			if e2 != nil {
				res.Msg = "领取请求失败：" + e2.Error()
			}
			break
		}
		var r struct {
			Status      string `json:"status"`
			FailureCode string `json:"failureCode"`
		}
		_ = json.Unmarshal(b2, &r)
		if r.Status == "CLAIMED" || r.Status == "SUCCESS" {
			res.Phase, res.Amount = "claimed", amount
			res.Msg = fmt.Sprintf("已领取 +%.0f Credits", amount)
		} else {
			res.Phase = "error"
			res.Msg = "服务端拒绝领取"
			if r.FailureCode != "" {
				res.Msg += "：" + r.FailureCode
			}
		}
	case isAlready:
		res.Phase, res.Amount = "already", amount
		res.Msg = fmt.Sprintf("今天已领取（+%.0f Credits）", amount)
	default:
		res.Phase = "pending"
		res.Msg = "服务端今天没有下发可领的每日额度"
	}
	return res
}

// qoderWebCheckinAll 用已保存的网页会话给每个账号领每日额度。
//
// 这条链路之前是断的：扫码登录会把 cookies.json 存下来，但**没有任何代码读它**
// （loadQoderWebCookies 零调用点）。于是「登录一次后每天自动签到」只是一句
// 承诺 —— 每天排程只跑 IDE 号池，网页会话账号根本不在其中。
func (p *Panel) qoderWebCheckinAll() qoderWebCheckinSummary {
	var sum qoderWebCheckinSummary
	root := qoderWebProfileRoot(p)
	metas := listQoderWebSessions(root)
	if len(metas) == 0 {
		return sum
	}
	for _, m := range metas {
		switch qoderWebCheckinOne(root, m).Phase {
		case "claimed":
			sum.Claimed++
		case "already":
			sum.Already++
		case "pending":
			sum.Pending++
		case "expired":
			sum.Expired++
		case "limited":
			sum.Limited++
		default:
			sum.Failed++
		}
	}
	log.Printf("panel: Qoder 网页会话签到完成：成功 %d / 今日已领 %d / 未下发 %d / 失败 %d / 已失效 %d / 限流 %d",
		sum.Claimed, sum.Already, sum.Pending, sum.Failed, sum.Expired, sum.Limited)
	return sum
}

// qoderWebCheckin 给指定网页会话签一次到（界面上单个账号的「签到」按钮）。
func (p *Panel) qoderWebCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		Session string `json:"session"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	root := qoderWebProfileRoot(p)
	metas := listQoderWebSessions(root)
	var hit *qoderWebMeta
	for i := range metas {
		m := &metas[i]
		byEmail := req.Email != "" && (strings.EqualFold(m.Email, req.Email) ||
			strings.EqualFold(m.SessionKey, req.Email) || strings.EqualFold(m.Dir, req.Email))
		bySession := req.Session != "" && (m.SessionKey == req.Session || m.Dir == req.Session)
		if byEmail || bySession {
			hit = m
			break
		}
	}
	if hit == nil {
		writeErr(w, http.StatusNotFound, "qoder_web_session_not_found")
		return
	}
	res := qoderWebCheckinOne(root, *hit)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     res.Phase == "claimed" || res.Phase == "already",
		"result": res,
	})
}

// qoderWebCheckinAllHTTP 手动「全部签到」：把网页会话也一起签一遍。
func (p *Panel) qoderWebCheckinAllHTTP(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "summary": p.qoderWebCheckinAll()})
}

// qoderWebDelete 删除网页会话账号（连同它的重复副本与浏览器 profile）。
//
// 会话文件只在 data/qoder-web 下，删完这个号就彻底没了：不再自动签到，
// 面板里也不再出现。要恢复只能重新扫码登录 —— 所以界面上是二次确认。
func (p *Panel) qoderWebDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		Session string `json:"session"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	target := strings.TrimSpace(firstNonEmpty(req.Email, req.Session))
	if target == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}
	root := qoderWebProfileRoot(p)
	metas := listQoderWebSessions(root)
	var hit *qoderWebMeta
	for i := range metas {
		m := &metas[i]
		if strings.EqualFold(m.Email, target) || m.SessionKey == target || m.Dir == target ||
			(m.UID != "" && strings.EqualFold(m.UID, target)) {
			hit = m
			break
		}
	}
	if hit == nil {
		writeErr(w, http.StatusNotFound, "qoder_web_session_not_found")
		return
	}
	dirs := append([]string{hit.Dir}, hit.DupDirs...)
	// 正在跑的浏览器先关掉：留着它会继续往刚删掉的 profile 里写 cookie。
	// 按目录（而不是会话键）匹配 —— 同一账号可能有 sso / 邮箱两个键在跑。
	live := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		live[filepath.Join(root, d)] = true
	}
	qoderWebSessions.mu.Lock()
	for k, st := range qoderWebSessions.by {
		if live[st.dir] || strings.EqualFold(k, hit.Email) || strings.EqualFold(k, hit.SessionKey) {
			qoderWebClose(st)
			delete(qoderWebSessions.by, k)
		}
	}
	qoderWebSessions.mu.Unlock()

	removed := make([]string, 0, len(dirs))
	for _, d := range dirs {
		// 目录名只可能是 shortHash 出来的 12 位 hex：不符就跳过，
		// 避免任何形式的路径穿越把 RemoveAll 带到数据目录外面。
		if !qoderWebDirNameOK(d) {
			log.Printf("panel: 拒绝删除可疑的 Qoder 会话目录 %q", d)
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, d)); err != nil {
			log.Printf("panel: 删除 Qoder 会话 %s 失败: %v", d, err)
			continue
		}
		removed = append(removed, d)
	}
	if len(removed) == 0 {
		writeErr(w, http.StatusInternalServerError, "qoder_web_delete_failed")
		return
	}
	label := firstNonEmpty(hit.Email, hit.Name, hit.Dir)
	log.Printf("panel: 删除 Qoder 网页会话账号 %s（目录 %v）", label, removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "email": hit.Email})
}

// qoderWebDirNameOK 校验会话目录名：只允许 12 位十六进制（shortHash 的输出）。
func qoderWebDirNameOK(name string) bool {
	if len(name) != 12 {
		return false
	}
	for _, r := range name {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
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

// qoderCookieNames 列出 qoder 域下的 Cookie 名字（只用于日志，不含值）。
func qoderCookieNames(all []cdpCookie) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range all {
		d := strings.ToLower(c.Domain)
		if !strings.Contains(d, "qoder") || c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// qoderBodySnippet 把响应体压成一行短串（日志用）。
func qoderBodySnippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
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

// qoderWebVisitorCookieNames 进站即有的访客 / 风控 Cookie 名单。
//
// 这些名字在「还没登录」时就已经存在（实测 2026-11-27 容器内 profile：
// cna / isg / tfstk / acw_tc / arms_uid / qoder_locale / qoder_visitor_id）。
// 用它们判断「有 Cookie ⇒ 已登录」会把扫码前当作已登录，于是一路打 401/429。
var qoderWebVisitorCookieNames = map[string]bool{
	"cna": true, "isg": true, "tfstk": true, "acw_tc": true, "arms_uid": true,
	"qoder_locale": true, "qoder_visitor_id": true, "qoder_theme": true,
	"_ga": true, "_gid": true, "_gat": true, "_ga_": true, "sensorsdata": true,
	"aliyun_qi": true, "aliyun_lang": true, "aliyun_country": true, "login_aliyunid_csrf": true,
}

// qoderWebHasSessionCookie 判断 qoder 域上是否出现「访客之外」的 Cookie。
//
// 只用作「可以试着打业务接口」的前置条件，不是登录成功的判据 ——
// 真正判据仍是 campaigns 返回 200。
func qoderWebHasSessionCookie(all []cdpCookie) bool {
	for _, c := range all {
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if !strings.Contains(host, "qoder") || c.Name == "" || c.Value == "" {
			continue
		}
		name := strings.ToLower(c.Name)
		if qoderWebVisitorCookieNames[name] {
			continue
		}
		if strings.HasPrefix(name, "_ga_") || strings.HasPrefix(name, "sensorsdata") || strings.HasPrefix(name, "arms_") {
			continue
		}
		return true
	}
	return false
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

// qoderWebGet / qoderWebPost 用网页会话调站点同源接口（CookieJar 自动按域挑 cookie）。
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
