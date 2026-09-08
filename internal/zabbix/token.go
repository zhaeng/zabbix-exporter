package zabbix

import (
	"context"
	"sync"
	"time"
)

// TokenInfo Token 信息管理
type TokenInfo struct {
	token     string
	expiresAt time.Time
	mu        sync.RWMutex
}

// NewTokenInfo 创建新的 TokenInfo
func NewTokenInfo() *TokenInfo {
	return &TokenInfo{}
}

// Set 设置 token
func (t *TokenInfo) Set(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = token
	// Zabbix token 默认 8 小时有效期，这里设置为 7 小时
	t.expiresAt = time.Now().Add(7 * time.Hour)
}

// Get 获取当前 token
func (t *TokenInfo) Get() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.token
}

// ExpiresAt 获取过期时间
func (t *TokenInfo) ExpiresAt() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.expiresAt
}

// IsExpired 检查 token 是否已过期
func (t *TokenInfo) IsExpired() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Now().After(t.expiresAt)
}

// ShouldRefresh 检查是否需要刷新（提前 5 分钟刷新）
func (t *TokenInfo) ShouldRefresh(advance time.Duration) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Now().Add(advance).After(t.expiresAt)
}

// TokenManager Token 管理器
type TokenManager struct {
	client          *Client
	refreshInterval time.Duration
	refreshAdvance  time.Duration
	mu              sync.Mutex
	isRunning       bool
	cancel          context.CancelFunc
	done            chan struct{}
}

// NewTokenManager 创建 Token 管理器
func NewTokenManager(client *Client, refreshInterval, refreshAdvance time.Duration) *TokenManager {
	return &TokenManager{
		client:          client,
		refreshInterval: refreshInterval,
		refreshAdvance:  refreshAdvance,
	}
}

// Start 启动 Token 管理器
func (m *TokenManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.isRunning {
		m.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.isRunning = true
	m.cancel = cancel
	m.done = make(chan struct{})
	done := m.done
	m.mu.Unlock()

	// 初始登录
	if err := m.client.Login(runCtx); err != nil {
		cancel()
		m.mu.Lock()
		m.isRunning = false
		m.mu.Unlock()
		close(done)
		return err
	}
	if err := runCtx.Err(); err != nil {
		m.mu.Lock()
		m.isRunning = false
		m.mu.Unlock()
		close(done)
		return err
	}

	// 启动定时刷新
	go m.runRefreshLoop(runCtx, done)

	return nil
}

// Stop 停止 Token 管理器
func (m *TokenManager) Stop() {
	_ = m.StopContext(context.Background())
}

// StopContext cancels an in-progress refresh and bounds the wait for the
// manager loop. Retry backoff and HTTP calls both observe the same context.
func (m *TokenManager) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if !m.isRunning {
		done := m.done
		m.mu.Unlock()
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.isRunning = false
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *TokenManager) runRefreshLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	if m.client.UsesAPIKey() {
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(m.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tokenInfo := m.client.GetTokenInfo()
			if tokenInfo.ShouldRefresh(m.refreshAdvance) {
				m.refreshWithRetry(ctx)
			}
		}
	}
}

func (m *TokenManager) refreshWithRetry(ctx context.Context) {
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for i := 0; i < 5; i++ {
		if err := m.client.RefreshToken(ctx); err != nil {
			if !waitTokenRetry(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		return
	}
}

func waitTokenRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// GetToken 获取当前 token
func (m *TokenManager) GetToken() string {
	return m.client.GetToken()
}
