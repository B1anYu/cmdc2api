// Package config 加载运行配置。环境变量优先，全部字段有安全默认值；
// 不读 config.json —— 容器部署走纯 env，状态（指纹等）单独落盘。
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port         int
	Host         string
	APIBase      string // cmdc 上游，默认 https://api.commandcode.ai
	StateFile    string // 指纹/生命周期节流状态持久化路径
	ZDR          bool   // 附加 x-cmd-zdr: 1 仅走 ZDR 路由
	MaxBodyBytes int64  // 入站请求体上限，默认 100MB
	ModelRefresh time.Duration
	IdleStream   time.Duration // 流式无新数据超时
	IdleBuffer   time.Duration // 非流式（缓冲聚合）超时

	// SessionStrategy 会话亲和策略：
	//   prefix（默认）— 无显式 session 头时，从 system+tools+首条用户消息
	//                   派生稳定会话 ID（同一对话跨轮次复用，命中 prompt cache）
	//   key            — 原版行为：每 key 12h+1h 抖动轮换
	SessionStrategy string
	// CacheMarkers 缓存断点策略：
	//   respect（默认）— 客户端 part 级标记透传，缺失时末尾合成兜底
	//   replace         — 剥掉客户端标记，强制末尾合成（A/B 诊断用）
	CacheMarkers string
	// AssistantReasoning 实验开关：把入站 assistant thinking 块以
	// {type:"reasoning"} 回传上游（cmdc 的历史 reasoning 形状未实弹验证，
	// 默认关闭即丢弃，与原版一致）。
	AssistantReasoning bool
	// FakeNodeVersion 信封 config.environment 里伪装的 Node 版本。
	FakeNodeVersion string
}

func Load() Config {
	c := Config{
		Port:              8050,
		Host:              "127.0.0.1",
		APIBase:           "https://api.commandcode.ai",
		StateFile:         "data/state.json",
		ZDR:               false,
		MaxBodyBytes:      100 * 1024 * 1024,
		ModelRefresh:      5 * time.Minute,
		IdleStream:        30 * time.Second,
		IdleBuffer:        90 * time.Second,
		SessionStrategy:   "prefix",
		CacheMarkers:      "respect",
		AssistantReasoning: false,
		FakeNodeVersion:   "v22.21.0",
	}
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Port = n
		}
	}
	if v := os.Getenv("HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("CC_API_BASE"); v != "" {
		c.APIBase = v
	}
	if v := os.Getenv("CC_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CMD_ZDR"); v == "1" {
		c.ZDR = true
	}
	if v := os.Getenv("CC_MAX_BODY_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxBodyBytes = n * 1024 * 1024
		}
	}
	if v := os.Getenv("CC_MODEL_REFRESH"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.ModelRefresh = d
		}
	}
	if v := os.Getenv("CC_SESSION_STRATEGY"); v == "key" || v == "prefix" {
		c.SessionStrategy = v
	}
	if v := os.Getenv("CC_CACHE_MARKERS"); v == "respect" || v == "replace" {
		c.CacheMarkers = v
	}
	if v := os.Getenv("CC_ASSISTANT_REASONING"); v == "1" {
		c.AssistantReasoning = true
	}
	if v := os.Getenv("CC_FAKE_NODE_VERSION"); v != "" {
		c.FakeNodeVersion = v
	}
	return c
}
