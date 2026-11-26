package panel

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const bundleTestAPIKey = "bundle-test-key"

func newBundleTestPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", filepath.Join(dir, "ide_accounts"))
	t.Setenv("MINIMAX_POOL_FILE", filepath.Join(dir, "minimax_accounts.json"))
	t.Setenv("DOUBAO_POOL_FILE", filepath.Join(dir, "doubao_accounts.json"))
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(dir, "codearts_accounts.json"))
	t.Setenv("QODER_DATA_DIR", filepath.Join(dir, "qoder_data"))
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	traeDir := filepath.Join(dir, "trae")
	p := New(Config{
		APIKey:      bundleTestAPIKey,
		AuthDir:     authDir,
		TraeDataDir: traeDir,
		Pool:        pool.New(""),
	})
	return p, dir
}

func bundleTestRequest(t *testing.T, p *Panel, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bundleTestAPIKey)
	p.ServeHTTP(rec, req)
	return rec
}

func TestBundleRoutesRegisteredAndProtected(t *testing.T) {
	p, _ := newBundleTestPanel(t)

	for _, path := range []string{"/panel/api/runtime", "/panel/api/export/bundle"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s 无鉴权应为 401，实际 %d", path, rec.Code)
		}
	}
	if rec := bundleTestRequest(t, p, http.MethodGet, "/panel/api/runtime", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET runtime 带鉴权应为 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := bundleTestRequest(t, p, http.MethodGet, "/panel/api/export/bundle", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET export bundle 带鉴权应为 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out credentialBundle
	if err := json.Unmarshal(bundleTestRequest(t, p, http.MethodGet, "/panel/api/export/bundle", "").Body.Bytes(), &out); err != nil {
		t.Fatalf("导出不是合法凭据包: %v", err)
	}
	if out.Format != bundleFormat || out.Version != bundleVersion {
		t.Fatalf("导出元信息不对: format=%q version=%d", out.Format, out.Version)
	}
}

func TestApplyBundleImportsEveryPoolWithoutLocalClient(t *testing.T) {
	p, dir := newBundleTestPanel(t)

	b := &credentialBundle{
		Format:  bundleFormat,
		Version: bundleVersion,
		GatewayAuth: []bundleAuth{{
			AccessToken: "access-1", RefreshToken: "refresh-1", UID: "uid-1",
			Nickname: "测试号", Realm: "cn",
		}},
		PrimaryIDE: []*ideauth.WorkBuddySnapshot{{
			Edition: "cn", Endpoint: "https://example.invalid",
			Account: &ideauth.WorkBuddyAccount{UID: "ide-1"},
		}},
		Qoder: []*ideauth.QoderSnapshot{{
			Email: "qoder@example.invalid", UserID: "q-1",
			Auth: &ideauth.QoderAuth{Token: "q-token", User: ideauth.QoderUser{Email: "qoder@example.invalid"}},
		}},
		TraeIDE: []*ideauth.TraeSnapshot{{
			Email: "trae@example.invalid", UserID: "t-1", Storage: `{"iCubeAuthInfo://icube.cloudide":"x"}`,
		}},
		TraeCheckin: []upstream.TraeAccount{{
			Name: "trae", UserID: "t-1", JWT: "jwt-value",
		}},
		MiniMax: []*minimax.Account{{
			ID: "mm-1", Region: "cn", AccessToken: "mm-access", RefreshToken: "mm-refresh",
		}},
		Doubao: []*doubao.Account{{
			ID: "db-1", SessionID: "session-1", Cookies: map[string]string{"sessionid": "session-1"},
		}},
		CodeArts: []bundleCodearts{{
			Label: "ca", Cookies: []codearts.Cookie{{Name: "SessionID", Value: "ca-session", Domain: ".huaweicloud.com"}},
		}},
	}

	result := p.applyBundle(context.Background(), b)
	if len(result.Errors) != 0 {
		t.Fatalf("导入不应报错: %v", result.Errors)
	}
	for _, platform := range []string{"gateway", "primary_ide", "qoder", "trae_ide", "trae_checkin", "minimax", "doubao", "codearts"} {
		if result.Imported[platform] != 1 {
			t.Errorf("%s 应导入 1 个，实际 %d", platform, result.Imported[platform])
		}
	}

	auths, err := auth.LoadDir(filepath.Join(dir, "auths"))
	if err != nil || len(auths) != 1 || auths[0].AccessToken != "access-1" {
		t.Fatalf("网关号池导入失败: len=%d err=%v", len(auths), err)
	}
	wbPool, err := ideauth.LoadWorkBuddyPool()
	if err != nil || len(wbPool.Accounts) != 1 {
		t.Fatalf("主 IDE 号池导入失败: len=%d err=%v", len(wbPool.Accounts), err)
	}
	qPool, err := ideauth.LoadQoderPool()
	if err != nil || len(qPool.Accounts) != 1 || qPool.Accounts[0].Auth.Token != "q-token" {
		t.Fatalf("Qoder 号池导入失败: len=%d err=%v", len(qPool.Accounts), err)
	}
	tPool, err := ideauth.LoadTraePool()
	if err != nil || len(tPool.Accounts) != 1 {
		t.Fatalf("Trae IDE 号池导入失败: len=%d err=%v", len(tPool.Accounts), err)
	}
	traeList, err := upstream.LoadTraeAccounts(filepath.Join(dir, "trae"))
	if err != nil || len(traeList) != 1 {
		t.Fatalf("Trae 签到号池导入失败: len=%d err=%v", len(traeList), err)
	}
	mmPool, err := minimax.LoadPool()
	if err != nil || len(mmPool.Accounts) != 1 || mmPool.Accounts[0].AccessToken != "mm-access" {
		t.Fatalf("MiniMax 号池导入失败: len=%d err=%v", len(mmPool.Accounts), err)
	}
	dbPool, err := doubao.LoadPool()
	if err != nil || len(dbPool.Accounts) != 1 || dbPool.Accounts[0].SessionID != "session-1" {
		t.Fatalf("豆包号池导入失败: len=%d err=%v", len(dbPool.Accounts), err)
	}
	caPool, err := codearts.LoadPool()
	if err != nil || len(caPool.Accounts) != 1 || len(caPool.Accounts[0].Cookies) != 1 {
		t.Fatalf("CodeArts 号池导入失败: len=%d err=%v", len(caPool.Accounts), err)
	}
}

func TestImportBundleHTTPMultipart(t *testing.T) {
	p, _ := newBundleTestPanel(t)

	body := &strings.Builder{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("file", "credentials-bundle.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	raw, err := json.Marshal(credentialBundle{
		Format: bundleFormat, Version: bundleVersion,
		MiniMax: []*minimax.Account{{ID: "mm-http", AccessToken: "mm-http-access", Region: "cn"}},
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/bundle", strings.NewReader(body.String()))
	req.Header.Set("Authorization", "Bearer "+bundleTestAPIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("导入应为 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK       bool           `json:"ok"`
		Imported map[string]int `json:"imported"`
		Errors   []string       `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析导入响应失败: %v", err)
	}
	if !out.OK || out.Imported["minimax"] != 1 || len(out.Errors) != 0 {
		t.Fatalf("导入响应不对: %+v", out)
	}
}
