// Package clients 提供 migrate 用的数据源 / 目标 HTTP 客户端。
package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// OpsKeeperClient 拉取 ops-keeper 数据。
//
// 假设 ops-keeper 暴露以下 REST endpoints（与 spec 一致）：
//
//	GET  /api/v1/{entity_type}                    列出
//	GET  /api/v1/{entity_type}/{id}               详情
//	POST /api/v1/{entity_type}:export             触发导出（job 模式）
//	GET  /api/v1/{entity_type}:export/{job_id}    查询导出状态
//
// 当前实现：使用 GET 单页拉取（适合中小规模）；后续可换 job 模式。
type OpsKeeperClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewOpsKeeperClient 创建客户端。
// baseURL 例："http://ops-keeper.internal:3000"
// token 例：Bearer token 或 API key。
func NewOpsKeeperClient(baseURL, token string) *OpsKeeperClient {
	return &OpsKeeperClient{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// ListEntity 列出某类实体的全部记录。
//
// 返回：rows + nextPageToken（无下一页时为空）。
// 分页大小默认 500。
func (c *OpsKeeperClient) ListEntity(
	ctx context.Context,
	entityType string,
	pageToken string,
	pageSize int,
) (rows []map[string]any, nextToken string, err error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 500
	}
	u, err := url.Parse(c.baseURL + "/api/v1/" + entityType)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("page_size", fmt.Sprintf("%d", pageSize))
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("ops-keeper 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("ops-keeper 返回 %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Items            []map[string]any `json:"items"`
		NextToken        string           `json:"next_token,omitempty"`
		OpsKeeperVersion string           `json:"opskeeper_version,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("解析 ops-keeper 响应失败: %w", err)
	}
	return payload.Items, payload.NextToken, nil
}

// ListAll 自动翻页拉取全部记录。
func (c *OpsKeeperClient) ListAll(ctx context.Context, entityType string) ([]map[string]any, error) {
	var all []map[string]any
	token := ""
	for {
		rows, next, err := c.ListEntity(ctx, entityType, token, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if next == "" {
			break
		}
		token = next

		// 简易节奏控制，避免压垮源
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return all, nil
}
