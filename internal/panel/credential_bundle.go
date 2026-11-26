// credential_bundle.go 跨环境迁移账号凭据。
//
// 容器里看不到桌面客户端的 %APPDATA% / ~/.minimax / DPAPI 密钥，也没有可交互
// 浏览器；因此「从本机客户端提取」这一类入口在 Docker 中天然不可用。
// 这里提供另一条路：在能访问客户端的本机面板导出一份 JSON 凭据包，把文件传到
// NAS 后在容器面板导入。导出/导入只处理号池里的结构化数据，不接收任何路径，
// 也不会在容器侧尝试解密客户端文件。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	bundleFormat   = "credential-bundle"
	bundleVersion  = 1
	maxBundleBytes = 64 << 20
)

// bundleAuth 是网关主号池里一份凭证的可传输表示。
// auth.Auth 内含互斥锁与未导出的 realm 字段，不能直接 JSON 往返。
type bundleAuth struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
	DeviceToken  string `json:"device_token"`
}

// bundleCodearts 只保留可跨机器使用的会话字段。
// 浏览器槽位、调试端口和本地 profile 路径都不带过来：容器里没有这些目录。
type bundleCodearts struct {
	Label          string            `json:"label,omitempty"`
	Identity       string            `json:"identity,omitempty"`
	Nickname       string            `json:"nickname,omitempty"`
	DomainID       string            `json:"domain_id,omitempty"`
	ClientSource   string            `json:"client_source,omitempty"`
	TicketDeadline string            `json:"ticket_deadline,omitempty"`
	TicketSource   string            `json:"ticket_source,omitempty"`
	Cookies        []codearts.Cookie `json:"cookies"`
}

// credentialBundle 是导出文件的顶层结构。
// 各平台字段彼此独立：缺少某个平台的号池不影响其它平台导入。
type credentialBundle struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	ExportedAt string `json:"exported_at"`

	GatewayAuth []bundleAuth                 `json:"gateway_auth,omitempty"`
	PrimaryIDE  []*ideauth.WorkBuddySnapshot `json:"primary_ide,omitempty"`
	Qoder       []*ideauth.QoderSnapshot     `json:"qoder,omitempty"`
	TraeIDE     []*ideauth.TraeSnapshot      `json:"trae_ide,omitempty"`
	TraeCheckin []upstream.TraeAccount       `json:"trae_checkin,omitempty"`
	MiniMax     []*minimax.Account           `json:"minimax,omitempty"`
	Doubao      []*doubao.Account            `json:"doubao,omitempty"`
	CodeArts    []bundleCodearts             `json:"codearts,omitempty"`
}

type bundleCounts struct {
	Imported map[string]int `json:"imported"`
	Skipped  map[string]int `json:"skipped"`
	Errors   []string       `json:"errors,omitempty"`
}

// localClientEnabled 决定面板是否展示「读取本机客户端」类入口。
// 显式环境变量优先；未设置时按操作系统推断（容器镜像会显式设为 0）。
func localClientEnabled() bool {
	if v := strings.TrimSpace(os.Getenv("PANEL_LOCAL_CLIENT")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// runtimeInfo 让前端知道当前是否是「无本机客户端」的运行环境。
func (p *Panel) runtimeInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"local_client":     localClientEnabled(),
		"bundle_supported": true,
	})
}

// exportBundle 把当前面板能读到的全部号池导出为可迁移 JSON。
//
//	GET /panel/api/export/bundle
func (p *Panel) exportBundle(w http.ResponseWriter, _ *http.Request) {
	b := credentialBundle{
		Format:     bundleFormat,
		Version:    bundleVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if dir := strings.TrimSpace(p.cfg.AuthDir); dir != "" {
		list, err := auth.LoadDir(dir)
		if err != nil {
			log.Printf("panel: export bundle 读取网关号池失败: %v", err)
		}
		for _, a := range list {
			if a == nil {
				continue
			}
			b.GatewayAuth = append(b.GatewayAuth, bundleAuth{
				AccessToken:  a.AccessToken,
				RefreshToken: a.RefreshToken,
				ExpiresAt:    a.ExpiresAt,
				Domain:       a.Domain,
				Realm:        a.RealmStored(),
				UID:          a.UID,
				EnterpriseID: a.EnterpriseID,
				Nickname:     a.Nickname,
				DeviceToken:  a.DeviceToken,
			})
		}
	}

	if pool, err := ideauth.LoadWorkBuddyPool(); err == nil {
		b.PrimaryIDE = append(b.PrimaryIDE, pool.Accounts...)
	} else {
		log.Printf("panel: export bundle 读取主 IDE 号池失败: %v", err)
	}
	if pool, err := ideauth.LoadQoderPool(); err == nil {
		b.Qoder = append(b.Qoder, pool.Accounts...)
	} else {
		log.Printf("panel: export bundle 读取 Qoder 号池失败: %v", err)
	}
	if pool, err := ideauth.LoadTraePool(); err == nil {
		b.TraeIDE = append(b.TraeIDE, pool.Accounts...)
	} else {
		log.Printf("panel: export bundle 读取 Trae IDE 号池失败: %v", err)
	}
	if list, err := upstream.LoadTraeAccounts(p.cfg.TraeDataDir); err == nil {
		b.TraeCheckin = append(b.TraeCheckin, list...)
	} else {
		log.Printf("panel: export bundle 读取 Trae 签到号池失败: %v", err)
	}
	if pool, err := minimax.LoadPool(); err == nil {
		b.MiniMax = append(b.MiniMax, pool.Accounts...)
	} else {
		log.Printf("panel: export bundle 读取 MiniMax 号池失败: %v", err)
	}
	if pool, err := doubao.LoadPool(); err == nil {
		b.Doubao = append(b.Doubao, pool.Accounts...)
	} else {
		log.Printf("panel: export bundle 读取豆包号池失败: %v", err)
	}
	if pool, err := codearts.LoadPool(); err == nil {
		for _, a := range pool.Accounts {
			if a == nil {
				continue
			}
			b.CodeArts = append(b.CodeArts, bundleCodearts{
				Label:          a.Label,
				Identity:       a.Identity,
				Nickname:       a.Nickname,
				DomainID:       a.DomainID,
				ClientSource:   a.ClientSource,
				TicketDeadline: a.TicketDeadline,
				TicketSource:   a.TicketSource,
				Cookies:        a.Cookies,
			})
		}
	} else {
		log.Printf("panel: export bundle 读取 CodeArts 号池失败: %v", err)
	}

	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化凭据包失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="credentials-bundle.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// importBundle 接收凭据包并逐平台写回号池。
//
//	POST /panel/api/import/bundle
//	Content-Type: multipart/form-data
//	Body: file=<credentials-bundle.json>
func (p *Panel) importBundle(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBundleBytes)
	if err := r.ParseMultipartForm(maxBundleBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传失败: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少文件字段 file: "+err.Error())
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBundleBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取文件失败: "+err.Error())
		return
	}

	var b credentialBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "凭据包不是合法 JSON: "+err.Error())
		return
	}
	if b.Format != "" && b.Format != bundleFormat {
		writeErr(w, http.StatusBadRequest, "不支持的凭据包格式: "+b.Format)
		return
	}
	if b.Version > bundleVersion {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("凭据包版本 %d 高于当前支持的版本 %d", b.Version, bundleVersion))
		return
	}

	result := p.applyBundle(r.Context(), &b)
	log.Printf("panel: credential bundle imported=%v skipped=%v", result.Imported, result.Skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": result.Imported,
		"skipped":  result.Skipped,
		"errors":   result.Errors,
	})
}

func newBundleCounts() bundleCounts {
	return bundleCounts{
		Imported: map[string]int{},
		Skipped:  map[string]int{},
	}
}

func (c *bundleCounts) skip(platform string, err error) {
	c.Skipped[platform]++
	c.Errors = append(c.Errors, platform+": "+err.Error())
}

func (p *Panel) applyBundle(ctx context.Context, b *credentialBundle) bundleCounts {
	res := newBundleCounts()

	for _, rec := range b.GatewayAuth {
		uid := strings.TrimSpace(rec.UID)
		if uid == "" || strings.TrimSpace(rec.AccessToken) == "" {
			res.skip("gateway", fmt.Errorf("缺少 uid 或 access_token"))
			continue
		}
		if !validImportUID(uid) {
			res.skip("gateway", fmt.Errorf("uid 不合法: %s", uid))
			continue
		}
		realm := auth.ResolveRealm(rec.Realm, rec.Domain)
		a := &auth.Auth{
			AccessToken:  strings.TrimSpace(rec.AccessToken),
			RefreshToken: strings.TrimSpace(rec.RefreshToken),
			ExpiresAt:    rec.ExpiresAt,
			Domain:       rec.Domain,
			UID:          uid,
			EnterpriseID: rec.EnterpriseID,
			Nickname:     rec.Nickname,
			DeviceToken:  rec.DeviceToken,
			FilePath:     filepath.Join(p.cfg.AuthDir, "workbuddy-"+uid+".json"),
		}
		if _, err := auth.BackfillRealmFor(a, realm); err != nil {
			res.skip("gateway", err)
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			res.skip("gateway", err)
			continue
		}
		if p.cfg.Pool != nil {
			p.cfg.Pool.Add(a)
			p.cfg.Pool.Revive(uid)
		}
		res.Imported["gateway"]++
	}

	if len(b.PrimaryIDE) > 0 {
		pool, err := ideauth.LoadWorkBuddyPool()
		if err != nil {
			res.skip("primary_ide", err)
		} else {
			for _, snap := range b.PrimaryIDE {
				if snap == nil || snap.Account == nil || strings.TrimSpace(snap.Account.UID) == "" {
					res.skip("primary_ide", fmt.Errorf("快照缺少账号 uid"))
					continue
				}
				key := snap.Account.UID + "|" + snap.Account.EnterpriseID + "|" + snap.Edition
				added := true
				for i, old := range pool.Accounts {
					if old == nil || old.Account == nil {
						continue
					}
					oldKey := old.Account.UID + "|" + old.Account.EnterpriseID + "|" + old.Edition
					if oldKey == key {
						pool.Accounts[i] = snap
						added = false
						break
					}
				}
				if added {
					pool.Accounts = append(pool.Accounts, snap)
				}
				res.Imported["primary_ide"]++
			}
			if err := ideauth.SaveWorkBuddyPool(pool); err != nil {
				res.skip("primary_ide", err)
			}
		}
	}

	if len(b.Qoder) > 0 {
		err := ideauth.UpdateQoderPool(func(pool *ideauth.QoderPoolFile) error {
			for _, snap := range b.Qoder {
				if snap == nil || strings.TrimSpace(snap.Email) == "" || snap.Auth == nil || strings.TrimSpace(snap.Auth.Token) == "" {
					res.skip("qoder", fmt.Errorf("快照缺少邮箱或 token"))
					continue
				}
				added := true
				for i, old := range pool.Accounts {
					if old != nil && strings.EqualFold(old.Email, snap.Email) {
						pool.Accounts[i] = snap
						added = false
						break
					}
				}
				if added {
					pool.Accounts = append(pool.Accounts, snap)
				}
				res.Imported["qoder"]++
			}
			return nil
		})
		if err != nil {
			res.skip("qoder", err)
		}
	}

	if len(b.TraeIDE) > 0 {
		pool, err := ideauth.LoadTraePool()
		if err != nil {
			res.skip("trae_ide", err)
		} else {
			for _, snap := range b.TraeIDE {
				if snap == nil || strings.TrimSpace(snap.Storage) == "" {
					res.skip("trae_ide", fmt.Errorf("快照缺少 storage"))
					continue
				}
				key := strings.ToLower(strings.TrimSpace(snap.Email))
				if key == "" {
					key = strings.TrimSpace(snap.UserID)
				}
				if key == "" {
					res.skip("trae_ide", fmt.Errorf("快照缺少账号标识"))
					continue
				}
				added := true
				for i, old := range pool.Accounts {
					if old == nil {
						continue
					}
					oldKey := strings.ToLower(strings.TrimSpace(old.Email))
					if oldKey == "" {
						oldKey = strings.TrimSpace(old.UserID)
					}
					if oldKey == key {
						pool.Accounts[i] = snap
						added = false
						break
					}
				}
				if added {
					pool.Accounts = append(pool.Accounts, snap)
				}
				res.Imported["trae_ide"]++
			}
			if err := ideauth.SaveTraePool(pool); err != nil {
				res.skip("trae_ide", err)
			}
		}
	}

	for _, acc := range b.TraeCheckin {
		if strings.TrimSpace(acc.JWT) == "" {
			res.skip("trae_checkin", fmt.Errorf("缺少 jwt"))
			continue
		}
		if strings.TrimSpace(acc.UserID) == "" {
			acc.UserID = upstream.TraeUserIDFromJWT(acc.JWT)
		}
		if strings.TrimSpace(acc.UserID) == "" {
			res.skip("trae_checkin", fmt.Errorf("无法从 jwt 解析 UserID"))
			continue
		}
		if _, err := upstream.SaveTraeAccount(p.cfg.TraeDataDir, acc); err != nil {
			res.skip("trae_checkin", err)
			continue
		}
		res.Imported["trae_checkin"]++
	}

	if len(b.MiniMax) > 0 {
		pool, err := minimax.LoadPool()
		if err != nil {
			res.skip("minimax", err)
		} else {
			for _, rec := range b.MiniMax {
				if rec == nil || strings.TrimSpace(rec.AccessToken) == "" {
					res.skip("minimax", fmt.Errorf("缺少 access_token"))
					continue
				}
				id := strings.TrimSpace(rec.ID)
				if id == "" {
					id = minimax.Fingerprint(rec.AccessToken)
				}
				added := true
				for i, old := range pool.Accounts {
					if old == nil {
						continue
					}
					if old.ID == id || old.AccessToken == rec.AccessToken ||
						(rec.UserID != "" && old.UserID == rec.UserID) {
						pool.Accounts[i] = rec
						added = false
						break
					}
				}
				if added {
					pool.Accounts = append(pool.Accounts, rec)
				}
				res.Imported["minimax"]++
			}
			if err := minimax.SavePool(pool); err != nil {
				res.skip("minimax", err)
			}
		}
	}

	if len(b.Doubao) > 0 {
		pool, err := doubao.LoadPool()
		if err != nil {
			res.skip("doubao", err)
		} else {
			for _, rec := range b.Doubao {
				if rec == nil || len(rec.Cookies) == 0 {
					res.skip("doubao", fmt.Errorf("缺少 cookies"))
					continue
				}
				id := strings.TrimSpace(rec.ID)
				if id == "" {
					sid := rec.Cookies["sessionid"]
					if sid == "" {
						sid = rec.Cookies["sessionid_ss"]
					}
					if sid == "" {
						res.skip("doubao", fmt.Errorf("cookies 缺少 sessionid"))
						continue
					}
					id = doubao.Fingerprint(sid)
				}
				rec.ID = id
				added := true
				for i, old := range pool.Accounts {
					if old == nil {
						continue
					}
					if old.ID == id || (rec.SessionID != "" && old.SessionID == rec.SessionID) {
						pool.Accounts[i] = rec
						added = false
						break
					}
				}
				if added {
					pool.Accounts = append(pool.Accounts, rec)
				}
				res.Imported["doubao"]++
			}
			if err := doubao.SavePool(pool); err != nil {
				res.skip("doubao", err)
			}
		}
	}

	if len(b.CodeArts) > 0 {
		err := codearts.UpdatePool(func(pool *codearts.Pool) error {
			for _, rec := range b.CodeArts {
				if len(rec.Cookies) == 0 {
					res.skip("codearts", fmt.Errorf("缺少 cookies"))
					continue
				}
				acc := &codearts.Account{
					ID:             codearts.Fingerprint(rec.Cookies),
					Identity:       rec.Identity,
					Nickname:       rec.Nickname,
					DomainID:       rec.DomainID,
					Cookies:        rec.Cookies,
					SavedAt:        time.Now().Format(time.RFC3339),
					ClientSource:   codearts.ClientSourceImport,
					TicketDeadline: rec.TicketDeadline,
					TicketSource:   rec.TicketSource,
				}
				if strings.TrimSpace(rec.Label) != "" {
					acc.Label = rec.Label
				}
				acc.Slot = acc.ID
				codearts.Upsert(pool, acc)
				res.Imported["codearts"]++
			}
			return nil
		})
		if err != nil {
			res.skip("codearts", err)
		}
	}

	return res
}
