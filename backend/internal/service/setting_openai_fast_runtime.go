package service

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// 短 TTL 限制其它实例或直接修改 DB 时的策略传播窗口。
	openAIFastPolicyRuntimeTTL     = 5 * time.Second
	openAIFastPolicyRuntimeTimeout = 5 * time.Second
)

// 快照发布后不可变；generation 阻止并发旧查询覆盖管理端刚保存的新策略。
type openAIFastPolicyRuntimeCache struct {
	writeMu    sync.Mutex
	mu         sync.RWMutex
	settings   *OpenAIFastPolicySettings
	expiresAt  time.Time
	generation uint64
	sf         singleflight.Group
}

type openAIFastPolicyRuntimeRead struct {
	settings   *OpenAIFastPolicySettings
	generation uint64
}

func cloneOpenAIFastPolicySettings(in *OpenAIFastPolicySettings) *OpenAIFastPolicySettings {
	if in == nil {
		return nil
	}
	out := &OpenAIFastPolicySettings{}
	if in.Rules != nil {
		out.Rules = append(make([]OpenAIFastPolicyRule, 0, len(in.Rules)), in.Rules...)
	}
	for i := range out.Rules {
		out.Rules[i].UserIDs = append([]int64(nil), in.Rules[i].UserIDs...)
		out.Rules[i].ModelWhitelist = append([]string(nil), in.Rules[i].ModelWhitelist...)
	}
	return out
}

func (s *SettingService) publishOpenAIFastPolicySettings(settings *OpenAIFastPolicySettings) {
	cache := &s.openAIFastPolicyRuntime
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.generation++
	cache.settings = cloneOpenAIFastPolicySettings(settings)
	cache.expiresAt = time.Now().Add(openAIFastPolicyRuntimeTTL)
}

// 仅热路径调用此方法。缓存未命中仍同步校验配置，不以无条件放行换取低延迟。
// 调用者取消可立即返回；共享回源使用独立且有上限的 context，避免取消污染其它请求。
func (s *SettingService) getOpenAIFastPolicySettingsCached(ctx context.Context) (*OpenAIFastPolicySettings, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cache := &s.openAIFastPolicyRuntime
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache.mu.RLock()
		generation := cache.generation
		if cache.settings != nil && time.Now().Before(cache.expiresAt) {
			result := cloneOpenAIFastPolicySettings(cache.settings)
			cache.mu.RUnlock()
			return result, nil
		}
		cache.mu.RUnlock()

		resultCh := cache.sf.DoChan(strconv.FormatUint(generation, 10), func() (any, error) {
			// 同一代晚到的请求复查缓存，避免上一回源刚完成又重复查库。
			cache.mu.RLock()
			if cache.generation != generation {
				cache.mu.RUnlock()
				return openAIFastPolicyRuntimeRead{generation: generation}, nil
			}
			if cache.settings != nil && time.Now().Before(cache.expiresAt) {
				result := openAIFastPolicyRuntimeRead{settings: cache.settings, generation: generation}
				cache.mu.RUnlock()
				return result, nil
			}
			cache.mu.RUnlock()

			dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIFastPolicyRuntimeTimeout)
			defer cancel()
			settings, err := s.GetOpenAIFastPolicySettings(dbCtx)
			cache.mu.Lock()
			if err == nil && cache.generation == generation {
				cache.settings = cloneOpenAIFastPolicySettings(settings)
				cache.expiresAt = time.Now().Add(openAIFastPolicyRuntimeTTL)
			}
			cache.mu.Unlock()
			// DB 错误不被永久缓存，维持原有调用者的错误处理约定。
			return openAIFastPolicyRuntimeRead{settings: settings, generation: generation}, err
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-resultCh:
			read, ok := result.Val.(openAIFastPolicyRuntimeRead)
			if !ok {
				// 回源结果异常时返回可诊断错误，不通过未检查的断言触发请求协程 panic。
				return nil, errors.New("invalid OpenAI Fast policy cache result")
			}
			cache.mu.RLock()
			currentGeneration := cache.generation
			cache.mu.RUnlock()
			if currentGeneration != read.generation {
				// 管理端在回源期间更新了配置，重新读取新代而不向请求返回旧策略。
				continue
			}
			return cloneOpenAIFastPolicySettings(read.settings), result.Err
		}
	}
}
