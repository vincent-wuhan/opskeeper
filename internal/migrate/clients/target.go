package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TargetClient 写入 opskeeper 数据。
//
// 通过 opskeeper 现有 REST API 写入，避免直接 SQL 操作。
// 端点（与 spec 对齐）：
//
//	POST   /api/v1/middleware                  创建中间件资源
//	POST   /api/v1/tenants                     创建 tenant
//	POST   /api/v1/users                       创建 user
//	POST   /api/v1/schedules                   创建 schedule
//	POST   /api/v1/alert-rules                 创建 alert rule
//	GET    /api/v1/{entity}/{id}               查询（幂等校验）
//	POST   /api/v1/{entity}/{id}:rollback      撤销（rollback snapshot 用）
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

// EntityExists 检查实体是否已存在（幂等校验）。
//
// 返回 true 表示存在，导入应跳过；false 表示需新建。
func (c *TargetClient) EntityExists(ctx context.Context, entityType, sourceID string) (bool, error) {
	// 通过 Idempotency-Key 头透传 ops-keeper source ID
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/"+entityType+"/by-source-id/"+sourceID, nil)
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
		c.baseURL+"/api/v1/"+entityType, bytes.NewReader(raw))
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
		c.baseURL+"/api/v1/"+entityType+"/"+id, nil)
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
