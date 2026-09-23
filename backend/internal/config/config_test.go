package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestConfig_Validate_Covers_RequiredRuntimeVars(t *testing.T) {
	f, err := os.Open("../../../deploy/required-runtime-vars.txt")
	if err != nil {
		t.Skipf("required-runtime-vars.txt not found, skipping cross-check: %v", err)
	}
	defer f.Close()

	requiredVars := make([]string, 0)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.SplitN(line, "#", 2)[0]
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		requiredVars = append(requiredVars, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("failed to read required-runtime-vars.txt: %v", err)
	}

	if len(requiredVars) == 0 {
		t.Fatal("required-runtime-vars.txt contains no variable entries")
	}

	for _, varName := range requiredVars {
		cfg := configWithAllFilledExcept(varName)
		err := cfg.validate()
		if err == nil {
			t.Errorf("validate() did NOT return error when %s is empty/unset — it must be checked", varName)
			continue
		}
		if !strings.Contains(err.Error(), varName) {
			t.Errorf("validate() error for %s should contain the variable name; got: %v", varName, err)
		}
	}
}

func configWithAllFilledExcept(emptyVar string) *Config {
	cfg := &Config{
		DatabaseURL:                 "postgres://x",
		RedisAddr:                   "redis://x",
		WechatAppID:                 "wx",
		WechatSecret:                "secret",
		WechatVirtualOfferID:        "offer",
		WechatVirtualAppKeyProd:     "prod-key",
		WechatVirtualAppKeySandbox:  "sandbox-key",
		WechatMsgToken:              "token",
		WechatVirtualCallbackToken:  "cb-token",
		WechatVirtualCallbackAESKey: "cb-aes-key",
		TencentMapKeys:              []string{"map-key"},
		AIAPIKey:                    "ai-key",
		APIHost:                     "api.example.com",
		DeploymentMode:              "saas",
		WorkerSecret:                "worker-secret",
		MCPWorkerSecret:             "mcp-worker-secret",
	}

	switch emptyVar {
	case "DATABASE_URL":
		cfg.DatabaseURL = ""
	case "REDIS_ADDR":
		cfg.RedisAddr = ""
	case "WECHAT_APPID":
		cfg.WechatAppID = ""
	case "WECHAT_SECRET":
		cfg.WechatSecret = ""
	case "TENCENT_MAP_KEY":
		cfg.TencentMapKeys = nil
	case "AI_API_KEY":
		cfg.AIAPIKey = ""
	case "WECHAT_VIRTUAL_OFFER_ID":
		cfg.WechatVirtualOfferID = ""
	case "WECHAT_VIRTUAL_APP_KEY_PRODUCTION":
		cfg.WechatVirtualAppKeyProd = ""
	case "WECHAT_VIRTUAL_APP_KEY_SANDBOX":
		cfg.WechatVirtualAppKeySandbox = ""
	case "API_HOST":
		cfg.APIHost = ""
	case "WORKER_SECRET":
		cfg.WorkerSecret = ""
	case "MCP_WORKER_SECRET":
		cfg.MCPWorkerSecret = ""
	case "WECHAT_MSG_TOKEN":
		cfg.WechatMsgToken = ""
	case "WECHAT_VIRTUAL_CALLBACK_TOKEN":
		cfg.WechatVirtualCallbackToken = ""
	case "WECHAT_VIRTUAL_CALLBACK_AES_KEY":
		cfg.WechatVirtualCallbackAESKey = ""
	}

	return cfg
}
