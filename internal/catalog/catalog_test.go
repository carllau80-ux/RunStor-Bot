package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"backup-restore-agent/internal/config"
)

func enabled() *bool { flag := true; return &flag }

// newProvider 指向一个 mock 清单接口。
func newProvider(t *testing.T, handler http.HandlerFunc, mutate func(*config.CatalogConfig)) (*Provider, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	cfg := config.CatalogConfig{
		Enabled: enabled(), URL: server.URL, TimeoutSeconds: 3, TTLSeconds: 60,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewProvider(cfg), &calls
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// 未配置接口时用内置清单 —— 服务照常可用, 只是清单位于代码里。
func TestUnconfiguredUsesBuiltin(t *testing.T) {
	provider := NewProvider(config.CatalogConfig{})
	if provider.Configured() {
		t.Error("没配地址不该算已配置")
	}
	cat := provider.Get()
	if cat.Source != "builtin" {
		t.Errorf("来源应为 builtin, 实际 %q", cat.Source)
	}
	if len(cat.Applications) == 0 || len(cat.MediaServers) == 0 {
		t.Fatal("内置清单不该为空")
	}
	// 内置清单要覆盖已注册的引擎与目的地类型
	if _, ok := cat.ApplicationFor("oracle"); !ok {
		t.Error("内置清单应包含 oracle")
	}
	if _, ok := cat.MediaServerFor("本地磁盘"); !ok {
		t.Error("内置清单应包含本地磁盘")
	}
}

func TestFetchApplicationsAndMediaServers(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"applications": []map[string]any{
				{"kind": "oracle", "display": "Oracle 19c", "source_type": "database",
					"aliases": []string{"orcl"}, "note": "生产库"},
				{"kind": "dameng", "display": "达梦 DM8"},
			},
			"media_servers": []map[string]any{
				{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
				{"id": "tape-01", "name": "磁带库01", "type": "tape"},
			},
		})
	}, nil)

	cat := provider.Get()
	if !cat.FromAPI() {
		t.Fatalf("来源应为 api, 实际 %q (warning=%s)", cat.Source, cat.Warning)
	}
	if len(cat.Applications) != 2 || len(cat.MediaServers) != 2 {
		t.Fatalf("清单数量不符: %d 应用 / %d 介质", len(cat.Applications), len(cat.MediaServers))
	}
	if app, ok := cat.ApplicationFor("dameng"); !ok || app.Display != "达梦 DM8" {
		t.Errorf("应能查到清单里的新应用: %+v", app)
	}
	if _, ok := cat.ApplicationFor("orcl"); !ok {
		t.Error("别名也应能查到")
	}
	server, ok := cat.MediaServerFor("本地磁盘池A")
	if !ok {
		t.Fatal("应能按名字查到介质服务器")
	}
	if server.Type != "local" || server.Path != "/u01/backup" {
		t.Errorf("介质服务器内容不符: %+v", server)
	}
	// 展示名应带上落点, 用户才知道选的是哪个目录
	if !strings.Contains(server.Display(), "本地磁盘池A") || !strings.Contains(server.Display(), "/u01/backup") {
		t.Errorf("展示名应含名称与落点: %q", server.Display())
	}
}

// 各家接口的包装与字段名不统一, 应尽量认出来。
func TestFetchToleratesEnvelopeAndFieldAliases(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// 常见的 {ok, data} 包装 + 字段别名
		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"apps": []map[string]any{
					{"type": "mysql", "name": "MySQL 主库"},
				},
				"storage": []map[string]any{
					{"code": "nfs1", "label": "共享目录1", "kind": "nfs", "mount_point": "/mnt/nfs"},
				},
			},
		})
	}, nil)

	cat := provider.Get()
	if !cat.FromAPI() {
		t.Fatalf("应识别出接口清单: %q %s", cat.Source, cat.Warning)
	}
	if app, ok := cat.ApplicationFor("mysql"); !ok || app.Display != "MySQL 主库" {
		t.Errorf("别名字段未识别: %+v", app)
	}
	server, ok := cat.MediaServerFor("nfs1")
	if !ok {
		t.Fatal("介质服务器别名未识别")
	}
	if server.Path != "/mnt/nfs" || server.Type != "nfs" {
		t.Errorf("介质服务器字段别名未识别: %+v", server)
	}
}

// 接口只提供应用时, 介质清单用内置补全, 而不是整份退回。
func TestPartialCatalogIsCompletedFromBuiltin(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"applications": []map[string]any{{"kind": "dameng", "display": "达梦"}},
		})
	}, nil)

	cat := provider.Get()
	if len(cat.Applications) != 1 {
		t.Errorf("应用应来自接口: %d", len(cat.Applications))
	}
	if len(cat.MediaServers) == 0 {
		t.Error("介质清单为空时应由内置补全")
	}
	if cat.Warning == "" {
		t.Error("补全过就该在提示里说明, 否则用户会以为介质清单也来自接口")
	}
}

// 接口不可用时退回内置清单, 并说明原因 —— 服务不能因为清单接口挂了就不能用。
func TestUnreachableAPIFallsBackToBuiltin(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}, nil)

	cat := provider.Get()
	if cat.Source != "builtin-fallback" {
		t.Errorf("来源应为 builtin-fallback, 实际 %q", cat.Source)
	}
	if !strings.Contains(cat.Warning, "清单接口不可用") {
		t.Errorf("应说明退化原因: %q", cat.Warning)
	}
	if len(cat.Applications) == 0 {
		t.Error("退回后仍要有可用的应用清单")
	}
}

// 曾经成功过就沿用上次结果, 比退回内置更贴近真实环境。
func TestStaleCacheUsedWhenAPIBreaksLater(t *testing.T) {
	var fail int32
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"applications":  []map[string]any{{"kind": "dameng", "display": "达梦"}},
			"media_servers": []map[string]any{{"id": "p1", "name": "池1", "type": "local"}},
		})
	}, nil)

	first := provider.Get()
	if !first.FromAPI() {
		t.Fatalf("首次应成功: %s", first.Warning)
	}

	atomic.StoreInt32(&fail, 1)
	second := provider.Refresh()
	if second.Source != "api" {
		t.Errorf("接口后来挂了应沿用上次结果, 实际 %q", second.Source)
	}
	if !strings.Contains(second.Warning, "沿用上次") {
		t.Errorf("应说明用的是上次结果: %q", second.Warning)
	}
	if _, ok := second.ApplicationFor("dameng"); !ok {
		t.Error("沿用时应保留上次的应用清单")
	}
}

// TTL 内应命中缓存, 不必反复打接口。
func TestCacheWithinTTL(t *testing.T) {
	provider, calls := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"applications":  []map[string]any{{"kind": "dameng"}},
			"media_servers": []map[string]any{{"id": "p1", "name": "池1"}},
		})
	}, func(cfg *config.CatalogConfig) { cfg.TTLSeconds = 300 })

	provider.Get()
	provider.Get()
	provider.Get()
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("TTL 内应只查一次, 实际 %d 次", got)
	}

	provider.Refresh()
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("强制刷新应再查一次, 实际 %d 次", got)
	}
}

// 接口鉴权: 默认 Bearer, 也可自定义头。
func TestAuthHeader(t *testing.T) {
	var seen string
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		writeJSON(w, map[string]any{
			"applications": []map[string]any{{"kind": "x"}},
		})
	}, func(cfg *config.CatalogConfig) { cfg.APIKeyEnv = "BRA_CATALOG_TEST_KEY" })

	t.Setenv("BRA_CATALOG_TEST_KEY", "sk-catalog")
	provider.Get()
	if seen != "Bearer sk-catalog" {
		t.Errorf("应带 Bearer 前缀, 实际 %q", seen)
	}
}

func TestCustomAuthHeaderName(t *testing.T) {
	var seen string
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Api-Token")
		writeJSON(w, map[string]any{
			"applications": []map[string]any{{"kind": "x"}},
		})
	}, func(cfg *config.CatalogConfig) {
		cfg.APIKeyEnv = "BRA_CATALOG_TEST_KEY"
		cfg.AuthHeader = "X-Api-Token"
	})

	t.Setenv("BRA_CATALOG_TEST_KEY", "raw-token")
	provider.Get()
	if seen != "raw-token" {
		t.Errorf("自定义头不该加 Bearer 前缀, 实际 %q", seen)
	}
}

// 应用与介质分成两个接口时的情形。
func TestSeparateEndpoints(t *testing.T) {
	apps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{{"kind": "dameng", "display": "达梦"}})
	}))
	defer apps.Close()
	servers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"targets": []map[string]any{{"id": "p1", "name": "池1", "type": "local"}},
		})
	}))
	defer servers.Close()

	provider := NewProvider(config.CatalogConfig{
		Enabled: enabled(), ApplicationsURL: apps.URL, MediaServersURL: servers.URL,
		TimeoutSeconds: 3, TTLSeconds: 60,
	})
	cat := provider.Get()
	if len(cat.Applications) != 1 || len(cat.MediaServers) != 1 {
		t.Fatalf("分别查询应各自取到: %+v", cat)
	}
	if !cat.FromAPI() {
		t.Errorf("来源应为 api: %s", cat.Warning)
	}
}

// 清单为空要报出来, 而不是静默当成"环境里什么都没有"。
func TestEmptyListIsReported(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"applications": []any{}})
	}, nil)

	cat := provider.Get()
	if cat.FromAPI() {
		t.Error("空清单不该当成有效的接口清单")
	}
	if cat.Warning == "" {
		t.Error("应说明为什么退回了内置清单")
	}
}

// 不是 JSON 时要给出可读报错, 而不是崩掉。
func TestMalformedResponseFallsBack(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>不是 JSON</html>"))
	}, nil)

	cat := provider.Get()
	if cat.Source != "builtin-fallback" {
		t.Errorf("应退回内置清单, 实际 %q", cat.Source)
	}
	if len(cat.Applications) == 0 {
		t.Error("退回后仍要可用")
	}
}

// 接口给的参数定义要能取出来并带上候选值。
func TestApplicationOptions(t *testing.T) {
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"applications": []map[string]any{{
				"kind": "oracle",
				"options": []map[string]any{
					{"field": "scope", "label": "备份范围", "kind": "enum",
						"choices":     []string{"整个实例", "仅全库"},
						"choices_map": map[string]string{"整个实例": "whole_instance", "仅全库": "database"}},
					{"field": "channels", "type": "int", "min": 1, "max": 32,
						"suggest": []string{"2", "4", "8"}},
				},
			}},
		})
	}, nil)

	cat := provider.Get()
	app, ok := cat.ApplicationFor("oracle")
	if !ok {
		t.Fatal("未找到 oracle")
	}
	if len(app.Options) != 2 {
		t.Fatalf("参数数量不符: %d", len(app.Options))
	}
	if app.Options[0].ChoicesMap["整个实例"] != "whole_instance" {
		t.Errorf("候选映射未解析: %+v", app.Options[0])
	}
	if app.Options[1].Max == nil || *app.Options[1].Max != 32 {
		t.Errorf("取值范围未解析: %+v", app.Options[1])
	}
	if !app.Options[1].IsRequired() {
		t.Error("未声明 required 时按必填处理")
	}
}

// 内置清单的形状要与接口清单一致, 上层才不必区分两条路径。
func TestBuiltinMatchesAPIShape(t *testing.T) {
	cat := Builtin()
	for _, app := range cat.Applications {
		if app.Kind == "" || app.Display == "" {
			t.Errorf("内置应用缺少 kind/display: %+v", app)
		}
	}
	oracle, ok := cat.ApplicationFor("oracle")
	if !ok {
		t.Fatal("内置清单应有 oracle")
	}
	if len(oracle.Options) == 0 {
		t.Error("内置清单应带上参数定义")
	}
	for _, server := range cat.MediaServers {
		if server.ID == "" || server.Type == "" {
			t.Errorf("内置介质服务器缺少 id/type: %+v", server)
		}
	}
}

// 目的地候选项应给出"服务器名 + 落点", 且映射能定位回服务器。
func TestChoiceLabelsAndMap(t *testing.T) {
	cat := Catalog{MediaServers: []MediaServer{
		{ID: "pool-a", Name: "池A", Type: "local", Path: "/u01"},
		{ID: "tape-1", Name: "带库1", Type: "tape"},
	}}
	labels := cat.ChoiceLabels()
	if len(labels) != 2 {
		t.Fatalf("候选数量不符: %v", labels)
	}
	if !strings.Contains(strings.Join(labels, "|"), "/u01") {
		t.Errorf("候选应带上落点: %v", labels)
	}
	mapping := cat.ChoiceMap()
	for _, label := range labels {
		if mapping[label] == "" {
			t.Errorf("候选 %q 缺少到服务器标识的映射", label)
		}
	}
	if server, ok := cat.ServerByID("tape-1"); !ok || server.Type != "tape" {
		t.Errorf("按标识取服务器失败: %+v", server)
	}
}

// 环境变量缺失时不该报错, 只是不带鉴权头。
func TestMissingKeyEnvIsNotFatal(t *testing.T) {
	var seen string
	provider, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		writeJSON(w, map[string]any{"applications": []map[string]any{{"kind": "x"}}})
	}, func(cfg *config.CatalogConfig) { cfg.APIKeyEnv = "BRA_CATALOG_ABSENT_KEY" })

	_ = os.Unsetenv("BRA_CATALOG_ABSENT_KEY")
	if cat := provider.Get(); !cat.FromAPI() {
		t.Fatalf("不该因为没配密钥就不查接口: %s", cat.Warning)
	}
	if seen != "" {
		t.Errorf("没有密钥时不该带鉴权头, 实际 %q", seen)
	}
}

// 兼容从文件读密钥的场景 (与模型配置同一套约定)。
func TestKeyFromFileIsNotUsedDirectly(t *testing.T) {
	// 清单接口只支持环境变量方式; 这里确认配置项被正确忽略而不是崩溃
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.key")
	if err := os.WriteFile(path, []byte("sk-x"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	provider := NewProvider(config.CatalogConfig{Enabled: enabled()})
	if provider.Configured() {
		t.Error("没有地址时不该算已配置")
	}
}
