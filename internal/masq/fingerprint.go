// Package masq 实现客户端伪装层：设备指纹、会话、CLI 版本号、
// lifecycle 预请求与上游请求头构造。
package masq

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Windows x64 平台的典型 CPU 型号与核心数组合。
var fingerprintCPUs = []struct {
	Model string
	Cores int
}{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var fingerprintMems = []int{8, 16, 24, 32, 48, 64}

var fingerprintTZs = []string{
	"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow",
	"Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong",
	"Australia/Sydney", "Pacific/Auckland",
}

// Fingerprint 与上游 /alpha/fingerprint/record 的请求体逐字段对应。
type Fingerprint struct {
	Thumbmark  string            `json:"thumbmark"`
	Components FingerprintComps  `json:"components"`
}

type FingerprintComps struct {
	MachineIDHash string   `json:"machineIdHash"`
	MACHashes     []string `json:"macHashes"`
	OSUserHash    string   `json:"osUserHash"`
	HostnameHash  string   `json:"hostnameHash"`
	GitEmailHash  string   `json:"gitEmailHash"`
	Platform      string   `json:"platform"`
	Arch          string   `json:"arch"`
	OSRelease     string   `json:"osRelease"`
	CPUModel      string   `json:"cpuModel"`
	CPUCount      int      `json:"cpuCount"`
	MemGiB        int      `json:"memGiB"`
	IsContainer   bool     `json:"isContainer"`
	Timezone      string   `json:"timezone"`
	Runtime       string   `json:"runtime"`
	CollectorVer  int      `json:"collectorVersion"`
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

// GenerateFingerprint 随机生成一份自洽的 win32/x64 设备指纹。
// thumbmark 是所有组件的联合哈希，按特定规范拼接计算以满足上游指纹一致性校验。
func GenerateFingerprint() Fingerprint {
	cpu := fingerprintCPUs[mrand.IntN(len(fingerprintCPUs))]
	memGiB := fingerprintMems[mrand.IntN(len(fingerprintMems))]
	tz := fingerprintTZs[mrand.IntN(len(fingerprintTZs))]
	macCount := 2 + mrand.IntN(4) // 2~5 个 MAC

	macHashes := make([]string, macCount)
	for i := range macHashes {
		macHashes[i] = sha256Hex([]byte(randHex(32)))
	}
	machineIDHash := sha256Hex([]byte(randHex(32)))
	osUserHash := sha256Hex([]byte(randHex(16)))
	hostnameHash := sha256Hex([]byte(randHex(16)))
	gitEmailHash := sha256Hex([]byte(randHex(16)))

	thumb := machineIDHash
	for _, m := range macHashes {
		thumb += "|" + m
	}
	thumb += "|" + osUserHash + "|" + hostnameHash + "|" + gitEmailHash +
		"|win32|10.0.22631|" + cpu.Model + "|" + itoa(cpu.Cores) + "|" + itoa(memGiB)

	return Fingerprint{
		Thumbmark: sha256Hex([]byte(thumb)),
		Components: FingerprintComps{
			MachineIDHash: machineIDHash,
			MACHashes:     macHashes,
			OSUserHash:    osUserHash,
			HostnameHash:  hostnameHash,
			GitEmailHash:  gitEmailHash,
			Platform:      "win32",
			Arch:          "x64",
			OSRelease:     "10.0.22631",
			CPUModel:      cpu.Model,
			CPUCount:      cpu.Cores,
			MemGiB:        memGiB,
			IsContainer:   false,
			Timezone:      tz,
			Runtime:       "cli",
			CollectorVer:  1,
		},
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// keyState 记录每个上游 key 的指纹与 lifecycle 节流时间。
type keyState struct {
	Fingerprint *Fingerprint `json:"fingerprint"`
	NextInitAt  time.Time    `json:"nextInitAt"`
}

// StateStore 把每个 API Key 的设备指纹持久化到磁盘，确保重启后指纹保持连续稳定。
// 索引键为 sha256(apiKey) 前缀，绝不持久化明文 Key。
type StateStore struct {
	mu   sync.Mutex
	path string
	Keys map[string]*keyState `json:"keys"`
}

func LoadState(path string) *StateStore {
	s := &StateStore{path: path, Keys: make(map[string]*keyState)}
	data, err := os.ReadFile(path)
	if err != nil {
		return s // 首次运行，无状态文件
	}
	if err := json.Unmarshal(data, s); err != nil {
		slog.Warn("state file corrupt, starting fresh", "error", err)
		s.Keys = make(map[string]*keyState)
	}
	return s
}

func keyHash(apiKey string) string {
	h := sha256.Sum256([]byte("cmdc2api:" + apiKey))
	return hex.EncodeToString(h[:8])
}

// KeyState 取（或首次生成）某个 key 的指纹与节流状态。
func (s *StateStore) KeyState(apiKey string) *keyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	kh := keyHash(apiKey)
	st, ok := s.Keys[kh]
	if !ok || st.Fingerprint == nil {
		fp := GenerateFingerprint()
		st = &keyState{Fingerprint: &fp, NextInitAt: time.Time{}}
		s.Keys[kh] = st
		s.saveLocked()
		slog.Info("fingerprint generated for key", "keyHash", kh)
	}
	return st
}

// ScheduleNextInit 记录下一次 lifecycle 预请求时间并落盘。
func (s *StateStore) ScheduleNextInit(apiKey string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.Keys[keyHash(apiKey)]; ok {
		st.NextInitAt = at
		s.saveLocked()
	}
}

// saveLocked 原子写（临时文件 + rename），崩溃不会留下半截 JSON。
func (s *StateStore) saveLocked() {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		slog.Warn("state dir create failed", "error", err)
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("state write failed", "error", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("state rename failed", "error", err)
	}
}
