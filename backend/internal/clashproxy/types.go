package clashproxy

import (
	"context"
	"time"
)

const defaultTestURL = "https://www.gstatic.com/generate_204"

type Options struct {
	Enabled                   bool
	MihomoBinaryPath          string
	RuntimeRoot               string
	StartupTimeout            time.Duration
	SubscriptionMaxBytes      int64
	AllowInsecureSubscription bool
	AllowPrivateSubscription  bool
}

// AccountProxyUpdater 通过现有账号服务更新 proxy_id，以复用缓存失效和影子账号同步逻辑。
type AccountProxyUpdater interface {
	SetAccountProxy(ctx context.Context, accountID int64, proxyID *int64) error
}

type NodeView struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	NodeType   string         `json:"node_type"`
	SourceType string         `json:"source_type"`
	Config     map[string]any `json:"config"`
	Status     string         `json:"status"`
}

type ProfileView struct {
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	Strategy        string         `json:"strategy"`
	TestURL         string         `json:"test_url"`
	IntervalSeconds int            `json:"interval_seconds"`
	Status          string         `json:"status"`
	AutoStart       bool           `json:"auto_start"`
	NodeIDs         []int64        `json:"node_ids"`
	ManagedProxyID  *int64         `json:"managed_proxy_id,omitempty"`
	Config          map[string]any `json:"config"`
}

type BindingView struct {
	ID              int64  `json:"id"`
	AccountID       int64  `json:"account_id"`
	AccountName     string `json:"account_name"`
	AccountPlatform string `json:"account_platform"`
	ProfileID       int64  `json:"profile_id"`
	ProfileName     string `json:"profile_name"`
	PreviousProxyID *int64 `json:"previous_proxy_id,omitempty"`
	Enabled         bool   `json:"enabled"`
}

type RuntimeView struct {
	ProfileID      int64  `json:"profile_id"`
	RuntimeType    string `json:"runtime_type"`
	PID            int    `json:"pid,omitempty"`
	MixedPort      int    `json:"mixed_port,omitempty"`
	ControllerPort int    `json:"controller_port,omitempty"`
	Status         string `json:"status"`
	LastError      string `json:"last_error,omitempty"`
	ProxyURL       string `json:"proxy_url,omitempty"`
}

type RuntimeStatusView struct {
	Total    int `json:"total"`
	Starting int `json:"starting"`
	Running  int `json:"running"`
	Failed   int `json:"failed"`
	Stopped  int `json:"stopped"`
}

type ProfileTestView struct {
	ProfileID int64  `json:"profile_id"`
	Healthy   bool   `json:"healthy"`
	Status    string `json:"status"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
	ProxyURL  string `json:"proxy_url,omitempty"`
}

type CreateNodeInput struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type ImportNodesInput struct {
	Format  string `json:"format"`
	Content string `json:"content"`
	URL     string `json:"url"`
}

type CreateProfileInput struct {
	Name            string  `json:"name"`
	Strategy        string  `json:"strategy"`
	TestURL         string  `json:"test_url"`
	IntervalSeconds int     `json:"interval_seconds"`
	AutoStart       bool    `json:"auto_start"`
	NodeIDs         []int64 `json:"node_ids"`
	Weights         []int   `json:"weights,omitempty"`
}

type UpdateProfileInput = CreateProfileInput

type CreateBindingInput struct {
	AccountID int64 `json:"account_id"`
	ProfileID int64 `json:"profile_id"`
}

// BulkBindOpenAIOAuthFailure 记录单个账号批量绑定失败，避免部分失败掩盖已完成结果。
type BulkBindOpenAIOAuthFailure struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}

// BulkBindOpenAIOAuthResult 返回本次批量绑定的可核对统计。
type BulkBindOpenAIOAuthResult struct {
	ProfileID int64                        `json:"profile_id"`
	Eligible  int                          `json:"eligible"`
	Bound     int                          `json:"bound"`
	Failed    int                          `json:"failed"`
	Failures  []BulkBindOpenAIOAuthFailure `json:"failures,omitempty"`
}
