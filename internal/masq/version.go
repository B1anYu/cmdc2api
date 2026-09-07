package masq

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// CCVersion 周期性从 npm registry 拉取官方 CLI（command-code）的最新版本号，
// 用于 x-command-code-version 头。版本号明显过旧是风控特征，必须跟随官方发版。
const (
	versionFallback     = "1.50.1" // 2026-09 npm 实测值；仅在 registry 不可达时兜底
	versionRefreshEvery = 24 * time.Hour
	versionEndpoint     = "https://registry.npmjs.org/command-code/latest"
)

type CCVersion struct {
	v atomic.Value // string
}

func NewCCVersion() *CCVersion {
	cv := &CCVersion{}
	cv.v.Store(versionFallback)
	return cv
}

func (c *CCVersion) Get() string { return c.v.Load().(string) }

// Start 启动即拉取一次，之后每 24h 刷新；失败静默沿用当前值。
func (c *CCVersion) Start() {
	refresh := func() {
		// 10s 超时，与原版 AbortSignal.timeout(10000) 对齐
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(versionEndpoint)
		if err != nil {
			slog.Warn("cc version fetch failed, using current", "version", c.Get(), "error", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			slog.Warn("cc version fetch non-200", "status", resp.StatusCode)
			return
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&pkg); err == nil && pkg.Version != "" {
			c.v.Store(pkg.Version)
			slog.Info("cc version refreshed", "version", pkg.Version)
		}
	}
	go func() {
		refresh()
		t := time.NewTicker(versionRefreshEvery)
		defer t.Stop()
		for range t.C {
			refresh()
		}
	}()
}
