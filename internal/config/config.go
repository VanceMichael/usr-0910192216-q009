// Package config 从环境变量读取服务配置。
package config

import "os"

// Config 是进程级配置。
type Config struct {
	DatabaseURL      string
	ListenAddr       string
	PlatformID       string
	EnableFaultHooks bool
	RulesFile        string
	VersionsFile     string
}

// FromEnv 读取环境变量，全部有默认值。
func FromEnv() Config {
	return Config{
		DatabaseURL:      getenv("DATABASE_URL", "postgres://app:app@localhost:5432/sharing"),
		ListenAddr:       getenv("LISTEN_ADDR", ":8080"),
		PlatformID:       getenv("PLATFORM_ID", "platform:ops"),
		EnableFaultHooks: getenv("LEDGER_FAULT_HOOKS", "") == "1",
		RulesFile: getenv("RULES_FILE", firstExisting(
			"/contracts/分账规则.json", "contracts/分账规则.json")),
		VersionsFile: getenv("VERSIONS_FILE", firstExisting(
			"/contracts/versions.json", "contracts/versions.json")),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[len(paths)-1]
}
