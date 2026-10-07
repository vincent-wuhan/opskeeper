package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TargetClient 写入 opskeeper 数据。
//
// 通过 opskeeper 现有 REST API 写入，避免直接 SQL 操作。
//
// entityType 是注册表里的 TargetRoute，形如 "/v1/users"，也就是 handler
// 自己注册的那一段；本客户端负责把它挂到 manager 的 "/api" 组下。改动
// 之前这里拼的是一段自由文本，于是 "tenants" / "schedules" /
// "middleware_resources" 三个名字被原样当成端点，而它们在路由表里没有
// 对应物（决策 291）。
//
// 真实端点（core/manager/iam/server/http.go 与 server/alert/http.go）：
//
//	POST   /api/v1/users         创建 user
//	POST   /api/v1/orgs          创建 org（原 tenants）
//	POST   /api/v1/alert-rules   创建 alert rule
//	GET    /api/v1/{route}/by-source-id/{id}   幂等查询
//	DELETE /api/v1/{route}/{id}                回滚删除
type TargetClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewTargetClient 创建 opskeeper 客户端。
func NewTargetClient(baseURL, token string) *TargetClient {
	return &TargetClient{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchEntity 按来源 ID 取回目标端上的那一条实体。
//
// verify 需要它来比对字段——只有"在不在"这个布尔值的话，一行数据被写错了
// 字段也照样算命中，那份 verify 报告就成了"数量对上了"而不是"内容对上了"。
// 决策 293 之前 FetchEntity 不存在，而 VerifyResult.FieldDiffs 是一个永远
// 为空的字段：报告的渲染代码会打印「字段差异: N」，可 N 恒为 0，
// 于是它读起来像"逐字段核对过且无差异"。
func (c *TargetClient) FetchEntity(ctx context.Context, entityType, sourceID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+apiPath(entityType, "by-source-id/"+sourceID), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTargetEntityNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("opskeeper 返回 %d: %s", resp.StatusCode, string(body))
	}
	var row map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		return nil, fmt.Errorf("解析 opskeeper 响应失败: %w", err)
	}
	return row, nil
}

// ErrTargetEntityNotFound 表示目标端没有这一条。
var ErrTargetEntityNotFound = errors.New("opskeeper 上没有这一条")

// EntityExists 检查实体是否已存在（幂等校验）。
//
// 返回 true 表示存在，导入应跳过；false 表示需新建。
func (c *TargetClient) EntityExists(ctx context.Context, entityType, sourceID string) (bool, error) {
	// 通过 Idempotency-Key 头透传 ops-keeper source ID
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+apiPath(entityType, "by-source-id/"+sourceID), nil)
	if err != nil {
		return false, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	body, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("opskeeper 返回 %d: %s", resp.StatusCode, string(body))
}

// CreateEntity 在 opskeeper 创建一条实体。
//
// reqBody 必须是已应用 FieldMap 后的目标格式。
// tenantID 由调用方从 TenantMapper.Map() 取得并注入到 body。
func (c *TargetClient) CreateEntity(
	ctx context.Context,
	entityType string,
	tenantID int64,
	reqBody map[string]any,
) (createdID string, err error) {
	// 注入 tenant_id
	reqBody["tenant_id"] = tenantID
	reqBody["migration_source"] = "opskeeper" // 标记来源，便于审计

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+apiPath(entityType, ""), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Idempotency: 用 ops-keeper 源 ID + entity 类型做幂等键
	if src, ok := reqBody["id"]; ok {
		req.Header.Set("Idempotency-Key",
			fmt.Sprintf("migrate:%s:%v", entityType, src))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("opskeeper POST %s 失败: %w", entityType, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var payload struct {
			Code int `json:"code"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", fmt.Errorf("解析 opskeeper 响应失败: %w (body: %s)", err, string(body))
		}
		return payload.Data.ID, nil
	}
	// 幂等命中：返回的 200/201 已含实体 → 解析 ID
	if resp.StatusCode == http.StatusConflict {
		// 实体已存在（幂等）
		return strings.TrimSpace(string(body)), nil
	}
	return "", fmt.Errorf("opskeeper 返回 %d: %s", resp.StatusCode, string(body))
}

// DeleteEntity 回滚单个实体。
//
// 用于 rollback 阶段：snapshot 记录原始 ID，回滚时按 ID 删除 opskeeper 实体。
func (c *TargetClient) DeleteEntity(ctx context.Context, entityType, id string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		c.baseURL+apiPath(entityType, id), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opskeeper DELETE %s/%s 失败: %w", entityType, id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil // 已删除，幂等
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("opskeeper DELETE %s/%s 返回 %d: %s", entityType, id, resp.StatusCode, string(body))
}

// HealthCheck 探活。
func (c *TargetClient) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opskeeper /healthz 返回 %d", resp.StatusCode)
	}
	return nil
}

// apiPath 把一条注册表里的路由加后缀拼成 manager 实际服务的路径。
//
// route 形如 "/v1/users"，manager 把所有 BC 挂在 "/api" 组下，所以结果是
// "/api/v1/users"。空后缀就是集合路径本身（POST 创建）。
func apiPath(route, suffix string) string {
	path := "/api" + route
	if suffix == "" {
		return path
	}
	return path + "/" + suffix
}
