package zabbix

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zabbix-exporter/internal/metrics"
)

type responseDecodeError struct {
	err error
}

func (e *responseDecodeError) Error() string { return e.err.Error() }
func (e *responseDecodeError) Unwrap() error { return e.err }

// Client Zabbix API 客户端
type Client struct {
	url        string
	username   string
	password   string
	usesAPIKey bool
	token      *TokenInfo
	httpClient *http.Client
	mu         sync.Mutex // 保护 token 刷新操作
}

// APIResponse Zabbix API 响应结构
type APIResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result"`
	Error   *APIError   `json:"error,omitempty"`
}

// APIError API 错误信息
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// LoginRequest 登录请求
type LoginRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"params"`
	ID   int `json:"id"`
	Auth any `json:"auth"`
}

// LoginResponse 登录响应
type LoginResponse struct {
	JSONRPC string `json:"jsonrpc"`
	Result  string `json:"result"` // token
	ID      int    `json:"id"`
}

// Item Zabbix 监控项
type Item struct {
	ItemID      string `json:"itemid"`
	HostID      string `json:"hostid"`
	Key         string `json:"key_"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ValueType   string `json:"value_type"`
	Delay       string `json:"delay,omitempty"`
	Status      string `json:"status,omitempty"`
	State       string `json:"state,omitempty"`
}

// Host Zabbix 主机
type Host struct {
	HostID     string      `json:"hostid"`
	Host       string      `json:"host"`
	Name       string      `json:"name"`
	Interfaces []Interface `json:"interfaces"`
	Inventory  any         `json:"inventory,omitempty"` // 可以是数组、对象或 null
}

// ParseInventory 解析主机资产信息
func (h *Host) ParseInventory() *HostInventory {
	if h.Inventory == nil {
		return nil
	}

	// 处理不同的 inventory 类型
	switch v := h.Inventory.(type) {
	case map[string]any:
		return parseInventoryFromMap(v)

	case string:
		if v == "" || v == "[]" || v == "{}" {
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(v), &data); err != nil {
			return nil
		}
		return parseInventoryFromMap(data)

	case []any:
		if len(v) == 0 {
			return nil
		}
		if len(v) > 0 {
			if m, ok := v[0].(map[string]any); ok {
				return parseInventoryFromMap(m)
			}
		}
		return nil

	case map[string]string:
		data := make(map[string]any)
		for k, val := range v {
			data[k] = val
		}
		return parseInventoryFromMap(data)

	default:
		return nil
	}
}

func parseInventoryFromMap(data map[string]any) *HostInventory {
	if len(data) == 0 {
		return nil
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil
	}

	var inventory HostInventory
	if err := json.Unmarshal(jsonData, &inventory); err != nil {
		return nil
	}

	return &inventory
}

// Interface Zabbix 主机接口
type Interface struct {
	InterfaceID string `json:"interfaceid"`
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	Port        string `json:"port"`
	Type        string `json:"type"`  // 1=agent, 2=snmp, 3=ipmi, 4=jmx
	Main        string `json:"main"`  // 1=main, 0=not main
	UseIP       string `json:"useip"` // 1=use IP, 0=use DNS
	Bulk        string `json:"bulk"`  // 1=bulk walk, 0=no bulk
}

// HostInventory 主机资产信息
type HostInventory struct {
	Type         string `json:"type,omitempty"`           // 主机类型
	TypeFull     string `json:"type_full,omitempty"`      // 完整类型名称
	Name         string `json:"name,omitempty"`           // 资产名称
	Alias        string `json:"alias,omitempty"`          // 别名
	OS           string `json:"os,omitempty"`             // 操作系统
	OSShort      string `json:"os_short,omitempty"`       // 操作系统简短名称
	OSFull       string `json:"os_full,omitempty"`        // 完整操作系统信息
	SerialNumber string `json:"serialno,omitempty"`       // 序列号
	Model        string `json:"model,omitempty"`          // 型号
	Vendor       string `json:"vendor,omitempty"`         // 厂商
	Hardware     string `json:"hardware,omitempty"`       // 硬件信息
	Software     string `json:"software,omitempty"`       // 软件信息
	SoftwareApp  string `json:"software_app_1,omitempty"` // 软件应用程序
	Contact      string `json:"contact,omitempty"`        // 联系人
	Location     string `json:"location,omitempty"`       // 位置
	LocationLat  string `json:"location_lat,omitempty"`   // 位置纬度
	LocationLon  string `json:"location_lon,omitempty"`   // 位置经度
	URL          string `json:"url,omitempty"`            // URL
	Description  string `json:"description,omitempty"`    // 描述
	Networks     string `json:"networks,omitempty"`       // 网络信息
	Notes        string `json:"notes,omitempty"`          // 备注
	MACAddress   string `json:"mac_address,omitempty"`    // MAC 地址
	CPU          string `json:"cpu,omitempty"`            // CPU 信息
	CPUCores     int    `json:"cpu_cores,omitempty"`      // CPU 核心数
	Memory       string `json:"memory,omitempty"`         // 内存信息
}

// HostGroup Zabbix 主机组
type HostGroup struct {
	GroupID string `json:"groupid"`
	Name    string `json:"name"`
}

// Template Zabbix 模板
type Template struct {
	TemplateID string `json:"templateid"`
	Host       string `json:"host"`
	Name       string `json:"name"`
}

// HistoryRequest 历史数据请求
type HistoryRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		History  int      `json:"history"`
		ItemIDs  []string `json:"itemids"`
		TimeFrom int64    `json:"time_from"`
		TimeTill int64    `json:"time_till"`
		Limit    int      `json:"limit"`
		Output   []string `json:"output"`
	} `json:"params"`
	Auth string `json:"auth"`
	ID   int    `json:"id"`
}

// HistoryResponse 历史数据响应
type HistoryResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	Result  []HistoryItem `json:"result"`
	ID      int           `json:"id"`
}

// FlexInt64 兼容字符串和数字两种格式的 int64
// 部分 Zabbix 版本的 history.get 返回的 clock 是字符串（如 "1754800000"）
type FlexInt64 int64

// UnmarshalJSON 兼容 JSON 字符串和数字两种格式
func (f *FlexInt64) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = FlexInt64(v)
	return nil
}

// HistoryItem 历史数据项
type HistoryItem struct {
	ItemID string    `json:"itemid"`
	Clock  FlexInt64 `json:"clock"`
	Value  string    `json:"value"`
	NS     FlexInt64 `json:"ns"`
}

// DiscoveryRule 自动发现规则 (LLD)
type DiscoveryRule struct {
	ItemID   string `json:"itemid"`
	HostID   string `json:"hostid"`
	Key      string `json:"key_"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Delay    string `json:"delay"`
	Lifetime string `json:"lifetime"`
}

// ItemPrototype 监控项原型
type ItemPrototype struct {
	ItemID      string `json:"itemid"`
	RuleID      string `json:"ruleid"`
	HostID      string `json:"hostid"`
	Key         string `json:"key_"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`
}

// NewClient 创建新的 Zabbix 客户端
func NewClient(url, username, password string) *Client {
	return NewClientWithTLS(url, username, password, false)
}

// NewClientWithTLS creates a user.login client and optionally disables TLS
// certificate verification for deployments using an untrusted internal CA.
func NewClientWithTLS(url, username, password string, tlsSkipVerify bool) *Client {
	return &Client{
		url:      url,
		username: username,
		password: password,
		token:    NewTokenInfo(),
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: zabbixTransport(tlsSkipVerify),
		},
	}
}

// NewAPIKeyClient creates a client authenticated by a long-lived Zabbix API
// token. API tokens are sent with the same Bearer header as login session
// tokens, but they must not be obtained or refreshed through user.login.
func NewAPIKeyClient(url, apiKey string) *Client {
	return NewAPIKeyClientWithTLS(url, apiKey, false)
}

// NewAPIKeyClientWithTLS creates an API key client with explicit TLS
// verification behavior.
func NewAPIKeyClientWithTLS(url, apiKey string, tlsSkipVerify bool) *Client {
	client := NewClientWithTLS(url, "", "", tlsSkipVerify)
	client.usesAPIKey = true
	client.token.Set(apiKey)
	return client
}

func zabbixTransport(tlsSkipVerify bool) http.RoundTripper {
	if !tlsSkipVerify {
		return nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicitly configured for internal/self-signed Zabbix endpoints
	return transport
}

// doRequest 发送 API 请求
func (c *Client) doRequest(ctx context.Context, method string, params any, auth string) (*json.RawMessage, error) {
	started := time.Now()
	status := "unknown_error"
	metrics.ZabbixAPIRequestsInFlight.WithLabelValues(method).Inc()
	defer func() {
		metrics.ZabbixAPIRequestsInFlight.WithLabelValues(method).Dec()
		metrics.ZabbixAPIRequestsTotal.WithLabelValues(method, status).Inc()
		metrics.ZabbixAPIRequestDuration.WithLabelValues(method).Observe(time.Since(started).Seconds())
	}()

	reqMap := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	}

	data, err := json.Marshal(reqMap)
	if err != nil {
		status = "encode_error"
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		status = "request_error"
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 使用 Authorization: Bearer header
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		status = apiTransportStatus(ctx, err)
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		status = apiTransportStatus(ctx, err)
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		status = "http_error"
		return nil, fmt.Errorf("zabbix api returned HTTP status %s", resp.Status)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		status = "decode_error"
		return nil, &responseDecodeError{err: fmt.Errorf("failed to unmarshal response: %w", err)}
	}

	if apiResp.Error != nil {
		status = "api_error"
		return nil, fmt.Errorf("zabbix api error: code=%d, message=%s, data=%s",
			apiResp.Error.Code, apiResp.Error.Message, apiResp.Error.Data)
	}
	status = "success"

	// 处理不同的响应类型：数组、对象、字符串
	switch v := apiResp.Result.(type) {
	case string:
		// 对于 user.login 等返回字符串的情况
		result := json.RawMessage(v)
		return &result, nil
	case json.RawMessage:
		return &v, nil
	case []any:
		// 对于数组类型
		data, _ := json.Marshal(v)
		result := json.RawMessage(data)
		return &result, nil
	case map[string]any:
		// 对于对象类型
		data, _ := json.Marshal(v)
		result := json.RawMessage(data)
		return &result, nil
	}

	return nil, nil
}

func apiTransportStatus(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "canceled"
	}
	return "transport_error"
}

// Login 登录获取 token
func (c *Client) Login(ctx context.Context) error {
	if c.usesAPIKey {
		if c.GetToken() == "" {
			return fmt.Errorf("zabbix api key is empty")
		}
		return nil
	}

	params := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{
		Username: c.username,
		Password: c.password,
	}

	result, err := c.doRequest(ctx, "user.login", params, "")
	if err != nil {
		return err
	}

	if result == nil {
		return fmt.Errorf("empty response from user.login")
	}

	// result 是 JSON 编码的字符串，直接转换为 string
	token := string(*result)
	// 去掉首尾的引号（JSON 字符串带引号）
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		token = token[1 : len(token)-1]
	}

	c.token.Set(token)
	return nil
}

// GetToken 获取当前有效的 token
func (c *Client) GetToken() string {
	return c.token.Get()
}

// RefreshToken 刷新 token（带锁保护，防止并发刷新）
func (c *Client) RefreshToken(ctx context.Context) error {
	if c.usesAPIKey {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Login(ctx)
}

// UsesAPIKey reports whether authentication is backed by a configured API
// token rather than a renewable user.login session.
func (c *Client) UsesAPIKey() bool {
	return c.usesAPIKey
}

// GetTokenInfo 获取 token 信息
func (c *Client) GetTokenInfo() *TokenInfo {
	return c.token
}

// GetHosts 获取所有主机
func (c *Client) GetHosts(ctx context.Context) ([]Host, error) {
	params := struct {
		Output           []string `json:"output"`
		SelectInterfaces string   `json:"selectInterfaces"`
		SelectInventory  string   `json:"selectInventory"`
	}{
		Output:           []string{"hostid", "host", "name"},
		SelectInterfaces: "extend",
		SelectInventory:  "extend",
	}

	result, err := c.doRequest(ctx, "host.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, fmt.Errorf("empty response from host.get")
	}

	var hosts []Host
	if err := json.Unmarshal(*result, &hosts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hosts: %w", err)
	}

	return hosts, nil
}

// GetHostsByGroupIDs 获取指定主机组下的主机
func (c *Client) GetHostsByGroupIDs(ctx context.Context, groupIDs []string) ([]Host, error) {
	if len(groupIDs) == 0 {
		return []Host{}, nil
	}

	params := struct {
		Output           []string `json:"output"`
		GroupIDs         []string `json:"groupids"`
		SelectInterfaces string   `json:"selectInterfaces"`
		SelectInventory  string   `json:"selectInventory"`
	}{
		Output:           []string{"hostid", "host", "name"},
		GroupIDs:         groupIDs,
		SelectInterfaces: "extend",
		SelectInventory:  "extend",
	}

	result, err := c.doRequest(ctx, "host.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var hosts []Host
	if err := json.Unmarshal(*result, &hosts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hosts: %w", err)
	}

	return hosts, nil
}

// GetHostGroups 获取主机组
func (c *Client) GetHostGroups(ctx context.Context) ([]HostGroup, error) {
	params := struct {
		Output []string `json:"output"`
	}{
		Output: []string{"groupid", "name"},
	}

	result, err := c.doRequest(ctx, "hostgroup.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var groups []HostGroup
	if err := json.Unmarshal(*result, &groups); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host groups: %w", err)
	}

	return groups, nil
}

// GetTemplates 获取所有模板
func (c *Client) GetTemplates(ctx context.Context) ([]Template, error) {
	params := struct {
		Output []string `json:"output"`
	}{
		Output: []string{"templateid", "host", "name"},
	}

	result, err := c.doRequest(ctx, "template.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var templates []Template
	if err := json.Unmarshal(*result, &templates); err != nil {
		return nil, fmt.Errorf("failed to unmarshal templates: %w", err)
	}

	return templates, nil
}

// GetItemsByTemplate 获取指定模板的监控项
func (c *Client) GetItemsByTemplate(ctx context.Context, templateID string) ([]Item, error) {
	params := struct {
		Output      []string `json:"output"`
		TemplateIDs []string `json:"templateids"`
		ValueTypes  []int    `json:"value_types"`
	}{
		Output:      []string{"itemid", "name", "key_", "description", "value_type"},
		TemplateIDs: []string{templateID},
		ValueTypes:  []int{0, 1, 3},
	}

	result, err := c.doRequest(ctx, "item.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var items []Item
	if err := json.Unmarshal(*result, &items); err != nil {
		return nil, fmt.Errorf("failed to unmarshal items: %w", err)
	}

	return items, nil
}

// GetDiscoveryRules 获取模板的自动发现规则 (LLD)
func (c *Client) GetDiscoveryRules(ctx context.Context, templateID string) ([]DiscoveryRule, error) {
	params := struct {
		Output      []string `json:"output"`
		TemplateIDs []string `json:"templateids"`
	}{
		Output:      []string{"itemid", "hostid", "key_", "name", "type", "delay", "lifetime"},
		TemplateIDs: []string{templateID},
	}

	result, err := c.doRequest(ctx, "discoveryrule.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var rules []DiscoveryRule
	if err := json.Unmarshal(*result, &rules); err != nil {
		return nil, fmt.Errorf("failed to unmarshal discovery rules: %w", err)
	}

	return rules, nil
}

// GetItemPrototypes 获取监控项原型
func (c *Client) GetItemPrototypes(ctx context.Context, ruleIDs []string) ([]ItemPrototype, error) {
	if len(ruleIDs) == 0 {
		return nil, nil
	}

	params := struct {
		Output      []string `json:"output"`
		RuleIDs     []string `json:"ruleids"`
		SelectHosts []string `json:"selectHosts"`
	}{
		Output:      []string{"itemid", "ruleid", "hostid", "key_", "name", "description", "type"},
		RuleIDs:     ruleIDs,
		SelectHosts: []string{"hostid"},
	}

	result, err := c.doRequest(ctx, "itemprototype.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	var prototypes []ItemPrototype
	if err := json.Unmarshal(*result, &prototypes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item prototypes: %w", err)
	}

	return prototypes, nil
}

// GetItemMetadata 获取指定主机的数值监控项定义。该低频 metadata 接口只请求
// 定义字段，明确不请求 lastvalue/lastclock；调用方负责分批、限流和刷新周期。
func (c *Client) GetItemMetadata(ctx context.Context, hostIDs []string) ([]Item, error) {
	if len(hostIDs) == 0 {
		return []Item{}, nil
	}

	params := struct {
		Output  []string       `json:"output"`
		HostIDs []string       `json:"hostids"`
		Filter  map[string]any `json:"filter"`
	}{
		Output:  []string{"itemid", "hostid", "key_", "name", "value_type", "delay", "status", "state"},
		HostIDs: hostIDs,
		Filter:  map[string]any{"value_type": []int{0, 3}}, // 0=numeric float, 3=numeric unsigned
	}

	result, err := c.doRequest(ctx, "item.get", params, c.GetToken())
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("empty response from metadata item.get")
	}

	var items []Item
	if err := json.Unmarshal(*result, &items); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item metadata: %w", err)
	}
	return items, nil
}
