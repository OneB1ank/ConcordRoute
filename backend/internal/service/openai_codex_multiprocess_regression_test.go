package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 两个真实测试子进程各有独立热缓存和账号锁；只把数据库替换为本地合成仓储。
// 仓储写入与生产 UpdateExtra 的 JSONB 顶层合并一致，不访问生产或模型。
type auditProcessRepo struct {
	AccountRepository
	base string
}

func (repo *auditProcessRepo) get(ctx context.Context, path string) (*Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repo.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("synthetic status %d", resp.StatusCode)
	}
	var result Account
	err = json.NewDecoder(resp.Body).Decode(&result)
	return &result, err
}

func (repo *auditProcessRepo) GetByID(ctx context.Context, _ int64) (*Account, error) {
	return repo.get(ctx, "/read")
}

func (repo *auditProcessRepo) UpdateExtra(ctx context.Context, _ int64, updates map[string]any) error {
	data, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repo.base+"/write", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("synthetic write status %d", resp.StatusCode)
	}
	return nil
}

type auditProcessSnapshot struct {
	Session string
	Thread  string
	Window  string
}

type auditProcessTransactionLease struct {
	Token   string  `json:"token"`
	Account Account `json:"account"`
}

type auditProcessTransactionCommit struct {
	Token   string         `json:"token"`
	Updates map[string]any `json:"updates,omitempty"`
}

func (repo *auditProcessRepo) transactionAction(ctx context.Context, path string, payload any, result any) error {
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repo.base+path, &body)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("synthetic transaction status %d", resp.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// WithCodexIdentityBindings 通过测试父进程持有的租约模拟跨应用实例数据库行锁。
func (repo *auditProcessRepo) WithCodexIdentityBindings(ctx context.Context, _ int64, prepare func(latest *Account) (map[string]any, error)) error {
	var lease auditProcessTransactionLease
	if err := repo.transactionAction(ctx, "/transaction/acquire", nil, &lease); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = repo.transactionAction(abortCtx, "/transaction/abort", auditProcessTransactionCommit{Token: lease.Token}, nil)
	}()
	updates, err := prepare(&lease.Account)
	if err != nil {
		return err
	}
	if err := repo.transactionAction(ctx, "/transaction/commit", auditProcessTransactionCommit{Token: lease.Token, Updates: updates}, nil); err != nil {
		return err
	}
	committed = true
	return nil
}

func TestCodexIdentityMultiProcessWorker(t *testing.T) {
	base := os.Getenv("CODEX_AUDIT_PROCESS_REPO")
	if base == "" {
		t.Skip("仅由多进程审计父测试启动")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := &auditProcessRepo{base: base}
	account, err := repo.get(ctx, "/fixture")
	require.NoError(t, err)
	body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 1, nil)
	ids, err := prepareCodexFingerprint(ctx, repo, account, func(local *Account) *codexFingerprintIDs {
		return resolveCodexFingerprintIDsFromRawRequest(local, http.Header{"Version": []string{"0.153.3"}}, body)
	})
	require.NoError(t, err)
	require.NotNil(t, ids)
	data, err := json.Marshal(auditProcessSnapshot{Session: ids.sessionID, Thread: ids.threadID, Window: ids.contextWindowID})
	require.NoError(t, err)
	fmt.Println("AUDIT_SNAPSHOT:" + string(data))
}

func TestCodexIdentityMultiProcessConcurrentFirstBinding(t *testing.T) {
	runCodexIdentityProcessPair(t, false)
}

func TestCodexIdentityMultiProcessExistingBindingControl(t *testing.T) {
	runCodexIdentityProcessPair(t, true)
}

func runCodexIdentityProcessPair(t *testing.T, warm bool) {
	t.Helper()
	fixture := newTestOAuthAccount(8877665501, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	auditDropIdentityHotState(fixture.ID)
	t.Cleanup(func() { auditDropIdentityHotState(fixture.ID) })
	if warm {
		repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(fixture)}
		body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 1, nil)
		_, err := prepareCodexFingerprint(context.Background(), repo, fixture, func(local *Account) *codexFingerprintIDs {
			return resolveCodexFingerprintIDsFromRawRequest(local, http.Header{"Version": []string{"0.153.3"}}, body)
		})
		require.NoError(t, err)
		fixture = auditCloneIdentityAccount(repo.stored)
	}
	stored := auditCloneIdentityAccount(fixture)
	var mu sync.Mutex
	reads, writes := 0, 0
	rowGate := make(chan struct{}, 1)
	rowGate <- struct{}{}
	activeToken := ""
	transactionSequence := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fixture)
	})
	mux.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		snapshot := auditCloneIdentityAccount(stored)
		reads++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(snapshot)
	})
	mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		var updates map[string]any
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		for key, value := range updates {
			stored.Extra[key] = value
		}
		writes++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/transaction/acquire", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-rowGate:
		case <-r.Context().Done():
			return
		}
		mu.Lock()
		transactionSequence++
		activeToken = fmt.Sprintf("tx-%d", transactionSequence)
		lease := auditProcessTransactionLease{Token: activeToken, Account: *auditCloneIdentityAccount(stored)}
		reads++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(lease)
	})
	releaseTransaction := func(token string, updates map[string]any) bool {
		mu.Lock()
		defer mu.Unlock()
		if token == "" || token != activeToken {
			return false
		}
		if len(updates) > 0 {
			for key, value := range updates {
				stored.Extra[key] = value
			}
			writes++
		}
		activeToken = ""
		rowGate <- struct{}{}
		return true
	}
	mux.HandleFunc("/transaction/commit", func(w http.ResponseWriter, r *http.Request) {
		var commit auditProcessTransactionCommit
		if err := json.NewDecoder(r.Body).Decode(&commit); err != nil || !releaseTransaction(commit.Token, commit.Updates) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/transaction/abort", func(w http.ResponseWriter, r *http.Request) {
		var commit auditProcessTransactionCommit
		if err := json.NewDecoder(r.Body).Decode(&commit); err != nil || !releaseTransaction(commit.Token, nil) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	outputs := make([][]byte, 2)
	errs := make([]error, 2)
	var workers sync.WaitGroup
	for index := range outputs {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCodexIdentityMultiProcessWorker$", "-test.v")
			cmd.Env = append(os.Environ(), "CODEX_AUDIT_PROCESS_REPO="+server.URL)
			outputs[index], errs[index] = cmd.CombinedOutput()
		}(index)
	}
	workers.Wait()
	snapshots := make([]auditProcessSnapshot, 2)
	for index, output := range outputs {
		require.NoError(t, errs[index], "%s", output)
		found := false
		for _, line := range strings.Split(string(output), "\n") {
			if encoded, ok := strings.CutPrefix(strings.TrimSpace(line), "AUDIT_SNAPSHOT:"); ok {
				require.NoError(t, json.Unmarshal([]byte(encoded), &snapshots[index]))
				found = true
			}
		}
		require.True(t, found, "子进程必须实际完成身份准备")
	}
	mu.Lock()
	durable := auditCloneIdentityAccount(stored)
	readCount, writeCount := reads, writes
	mu.Unlock()
	matches := 0
	for _, snapshot := range snapshots {
		present := map[string]bool{}
		for _, value := range readCodexIdentityBindings(durable) {
			if binding, ok := parseCodexIdentityBinding(value); ok {
				present[binding.UUID] = true
			}
		}
		if present[snapshot.Session] && present[snapshot.Thread] && present[snapshot.Window] {
			matches++
		}
	}
	t.Logf("MULTIPROCESS reads=%d writes=%d accepted=%d durable_matches=%d same_session=%t same_thread=%t same_window=%t",
		readCount, writeCount, len(snapshots), matches,
		snapshots[0].Session == snapshots[1].Session, snapshots[0].Thread == snapshots[1].Thread,
		snapshots[0].Window == snapshots[1].Window)
	require.Equal(t, snapshots[0], snapshots[1], "同账号同客户端逻辑标识的成功出站快照应一致")
	require.Equal(t, len(snapshots), matches, "成功返回的快照应与落库值保持一致")
}
