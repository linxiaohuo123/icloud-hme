/**
 * [INPUT]: 依赖 context, errors, fmt, sort, strconv, strings, time, github.com/tidwall/gjson
 * [OUTPUT]: 对外提供 Alias, CreateResult, ListAliases, ListAliasesWithContext, Generate, GenerateWithContext, Reserve, ReserveWithContext, CreateAlias, CreateAliasWithContext, DeactivateHME, ReactivateHME, Delete, UpdateMetaData
 * [POS]: internal/hme 的 HME 别名协议操作层，负责别名生成、保留、列出、激活、修改、删除与状态核对
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package hme

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Alias 是一个 Hide My Email 隐私邮箱别名。
type Alias struct {
	Email       string `json:"email"`
	AnonymousID string `json:"anonymousId"`
	Label       string `json:"label"`
	Active      bool   `json:"active"`
	CreatedAt   string `json:"createdAt,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
}

// CreateResult 是 CreateAlias 的返回结果。
type CreateResult struct {
	Email       string `json:"email"`
	AnonymousID string `json:"anonymousId,omitempty"`
	Label       string `json:"label"`
	CreatedAt   string `json:"created_at"`
}

// ListAliasesWithContext 列出当前账号所有 Hide My Email 别名 (支持 Context 贯穿与严格模式)。
func (c *Client) ListAliasesWithContext(ctx context.Context) ([]Alias, error) {
	if err := c.resolveService(ctx); err != nil {
		return nil, err
	}
	c.log("获取别名列表...")
	body, err := c.RequestWithContext(ctx, "GET", c.ServiceURL()+"/v2/hme/list", nil, 0, MaxRetries)
	if err != nil {
		if errors.Is(err, ErrAuthFailed) || ctx.Err() != nil {
			return nil, err
		}
		// 若已缓存的 serviceURL 失效，清空并重新走一次 ValidateSession 自愈
		if c.ServiceURL() != "" {
			c.ResetServiceEndpoint()
			if resolveErr := c.resolveService(ctx); resolveErr == nil {
				body, err = c.RequestWithContext(ctx, "GET", c.ServiceURL()+"/v2/hme/list", nil, 0, MaxRetries)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	aliases, err := parseAliasList(body)
	if err != nil {
		return nil, err
	}
	c.log("共 %d 个别名", len(aliases))
	return aliases, nil
}

// ListAliases 列出当前账号所有 Hide My Email 别名。
func (c *Client) ListAliases() ([]Alias, error) {
	return c.ListAliasesWithContext(context.Background())
}

// GenerateWithContext 生成一个候选别名(尚未保留,需再调用 ReserveWithContext)。
func (c *Client) GenerateWithContext(ctx context.Context) (string, error) {
	if err := c.resolveService(ctx); err != nil {
		return "", err
	}
	c.log("生成候选别名...")
	body, err := c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/generate", map[string]string{"langCode": "en-us"}, 0, 2)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(body)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") || !gjson.Valid(body) {
		return "", fmt.Errorf("%w: invalid generate response schema", ErrInvalidResponseSchema)
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		errMsg := parsed.Get("error.errorMessage").String()
		return "", fmt.Errorf("生成失败: %s", nonEmpty(errMsg, "unknown"))
	}
	hmeResult := parsed.Get("result.hme")
	hme := ""
	if hmeResult.IsObject() {
		hme = hmeResult.Get("hme").String()
		if hme == "" {
			hme = hmeResult.Get("email").String()
		}
	} else {
		hme = hmeResult.String()
	}
	if hme == "" {
		return "", fmt.Errorf("%w: empty candidate email in response", ErrInvalidResponseSchema)
	}
	c.log("候选: %s", hme)
	return hme, nil
}

// Generate 生成一个候选别名(尚未保留,需再调用 Reserve)。
func (c *Client) Generate() (string, error) {
	return c.GenerateWithContext(context.Background())
}

// reserveInternalWithContext 保留候选别名并返回真实 email 和 anonymousId。
func (c *Client) reserveInternalWithContext(ctx context.Context, hme, label string) (string, string, error) {
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	if err := c.resolveService(ctx); err != nil {
		return "", "", err
	}
	if label == "" {
		label = "Created " + time.Now().Format("2006-01-02 15:04")
	}
	c.log("保留别名 %s ...", hme)
	payload := map[string]string{
		"hme":   hme,
		"label": label,
		"note":  "Created by icloud_hme tool",
	}

	reconcile := func(triggerErr error) (string, string, error) {
		c.log("Reserve 请求返回异常/未决状态 (%v)，启动上游一致性核对...", triggerErr)
		// 使用独立 5 秒 timeout 上下文，防止因外层 ctx 超时/取消而中断核对
		reconcileCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rCancel()

		aliases, listErr := c.ListAliasesWithContext(reconcileCtx)
		if listErr == nil {
			for _, a := range aliases {
				if strings.EqualFold(a.Email, hme) {
					c.log("核对恢复成功: 候选 %s 已存在于上游列表 (id: %s)", hme, a.AnonymousID)
					return a.Email, a.AnonymousID, nil
				}
			}
			return "", "", fmt.Errorf("%w: reserve inconclusive (%v) and candidate %s not found in upstream list", ErrOutcomeUnknown, triggerErr, hme)
		}
		return "", "", fmt.Errorf("%w (reconciliation failed: %v): %v", ErrOutcomeUnknown, listErr, triggerErr)
	}

	// 写操作必须 maxAttempts=1，严禁通用盲目重试导致重复保留
	body, err := c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/reserve", payload, 0, 1)
	if err != nil {
		if errors.Is(err, ErrAuthFailed) {
			return "", "", err
		}
		return reconcile(err)
	}

	trimmed := strings.TrimSpace(body)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") || !gjson.Valid(body) {
		return reconcile(fmt.Errorf("%w: invalid reserve response schema", ErrInvalidResponseSchema))
	}

	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		if parsed.Get("error").Exists() {
			errMsg := parsed.Get("error.errorMessage").String()
			return "", "", fmt.Errorf("保留失败: %s", nonEmpty(errMsg, "unknown"))
		}
		return reconcile(fmt.Errorf("%w: reserve response success=false without standard error", ErrInvalidResponseSchema))
	}
	alias := hme
	var anonymousID string
	resultHme := parsed.Get("result.hme")
	if resultHme.IsObject() {
		if v := resultHme.Get("hme").String(); v != "" {
			alias = v
		}
		anonymousID = firstNonEmpty(resultHme.Get("anonymousId").String(), resultHme.Get("id").String())
	}
	c.log("已保留: %s (id: %s)", alias, anonymousID)
	return alias, anonymousID, nil
}

// ReserveWithContext 保留/确认候选别名,使其正式生效 (写操作 maxAttempts=1，包含网络中断后的写入状态核对)。
func (c *Client) ReserveWithContext(ctx context.Context, hme, label string) (string, error) {
	email, _, err := c.reserveInternalWithContext(ctx, hme, label)
	return email, err
}

// Reserve 保留/确认候选别名,使其正式生效。
func (c *Client) Reserve(hme, label string) (string, error) {
	return c.ReserveWithContext(context.Background(), hme, label)
}

// CreateAliasWithContext 一步完成「生成 + 保留」,创建一个新别名 (支持 Context 贯穿与状态机防护)。
func (c *Client) CreateAliasWithContext(ctx context.Context, label string, maxRetries int) (*CreateResult, error) {
	if maxRetries <= 0 {
		maxRetries = 5
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if attempt > 0 {
			c.ResetServiceEndpoint()
			c.log("重试 %d/%d ...", attempt+1, maxRetries)
		}
		hme, err := c.GenerateWithContext(ctx)
		if err != nil {
			lastErr = fmt.Errorf("generate 失败: %w", err)
			c.log("%s", lastErr)
			if errors.Is(err, ErrAuthFailed) {
				return nil, err
			}
			if attempt < maxRetries-1 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Second):
					continue
				}
			}
			break
		}
		email, anonID, err := c.reserveInternalWithContext(ctx, hme, label)
		if err != nil {
			lastErr = fmt.Errorf("reserve 失败: %w", err)
			c.log("%s", lastErr)
			if errors.Is(err, ErrAuthFailed) {
				return nil, err
			}
			// 若结果不明，不可盲目生成新候选覆盖，立即返回避免重复占号
			if errors.Is(err, ErrOutcomeUnknown) {
				return nil, lastErr
			}
			if attempt < maxRetries-1 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Second):
					continue
				}
			}
			break
		}
		return &CreateResult{
			Email:       email,
			AnonymousID: anonID,
			Label:       label,
			CreatedAt:   time.Now().Format(time.RFC3339),
		}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("创建别名失败: %w", lastErr)
	}
	return nil, fmt.Errorf("创建别名失败,已重试 %d 次", maxRetries)
}

// CreateAlias 一步完成「生成 + 保留」,创建一个新别名。
func (c *Client) CreateAlias(label string, maxRetries int) (*CreateResult, error) {
	return c.CreateAliasWithContext(context.Background(), label, maxRetries)
}

// DeactivateHMEWithContext 停用别名(可恢复)。
func (c *Client) DeactivateHMEWithContext(ctx context.Context, anonymousID string) (bool, error) {
	if err := c.resolveService(ctx); err != nil {
		return false, err
	}
	c.log("停用 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/deactivate", payload, 0, 1)
	if err != nil {
		return false, err
	}
	return gjson.Get(body, "success").Bool(), nil
}

// DeactivateHME 停用别名(可恢复)。
func (c *Client) DeactivateHME(anonymousID string) (bool, error) {
	return c.DeactivateHMEWithContext(context.Background(), anonymousID)
}

// ReactivateHMEWithContext 激活已停用的别名。
func (c *Client) ReactivateHMEWithContext(ctx context.Context, anonymousID string) (bool, error) {
	if err := c.resolveService(ctx); err != nil {
		return false, err
	}
	c.log("激活 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/reactivate", payload, 0, 1)
	if err != nil {
		return false, err
	}
	return gjson.Get(body, "success").Bool(), nil
}

// ReactivateHME 激活已停用的别名。
func (c *Client) ReactivateHME(anonymousID string) (bool, error) {
	return c.ReactivateHMEWithContext(context.Background(), anonymousID)
}

// DeleteWithContext 删除别名。若直接删除失败会先停用再删。
func (c *Client) DeleteWithContext(ctx context.Context, anonymousID string) error {
	if err := c.resolveService(ctx); err != nil {
		return err
	}
	c.log("删除 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	doDelete := func() (string, error) {
		return c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/delete", payload, 0, 1)
	}
	body, err := doDelete()
	if err != nil || !gjson.Get(body, "success").Bool() {
		c.log("直接删除失败,尝试先停用...")
		_, _ = c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/deactivate", payload, 0, 1)
		body, err = doDelete()
		if err != nil {
			return err
		}
		if !gjson.Get(body, "success").Bool() {
			return fmt.Errorf("%s", gjson.Get(body, "error.errorMessage").String())
		}
	}
	c.log("已删除")
	return nil
}

// Delete 删除别名。若直接删除失败会先停用再删。
func (c *Client) Delete(anonymousID string) error {
	return c.DeleteWithContext(context.Background(), anonymousID)
}

// UpdateMetaDataWithContext 更新别名备注 (label) 与说明 (note)。
func (c *Client) UpdateMetaDataWithContext(ctx context.Context, anonymousID, label, note string) error {
	if err := c.resolveService(ctx); err != nil {
		return err
	}
	c.log("更新别名备注 %s -> %s ...", anonymousID, label)
	payload := map[string]string{
		"anonymousId": anonymousID,
		"label":       label,
		"note":        note,
	}
	body, err := c.RequestWithContext(ctx, "POST", c.ServiceURL()+"/v1/hme/updateMetaData", payload, 0, 1)
	if err != nil {
		return err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		errMsg := parsed.Get("error.errorMessage").String()
		return fmt.Errorf("更新备注失败: %s", nonEmpty(errMsg, "unknown"))
	}
	c.log("备注已更新")
	return nil
}

// UpdateMetaData 更新别名备注 (label) 与说明 (note)。
func (c *Client) UpdateMetaData(anonymousID, label, note string) error {
	return c.UpdateMetaDataWithContext(context.Background(), anonymousID, label, note)
}

// ---- 别名列表解析 (对应 ICloudHME._parse_alias_list) ----

// parseAliasList 解析 iCloud 返回的别名列表 JSON。
// 严防格式假成功: 若收到 HTML、无效 JSON、success=false 或缺失关键数组，必须返回 ErrInvalidResponseSchema。
func parseAliasList(body string) ([]Alias, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty response body", ErrInvalidResponseSchema)
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<head") {
		return nil, fmt.Errorf("%w: received HTML page instead of JSON", ErrInvalidResponseSchema)
	}
	if !gjson.Valid(body) {
		return nil, fmt.Errorf("%w: invalid JSON format", ErrInvalidResponseSchema)
	}
	root := gjson.Parse(body)

	successVal := root.Get("success")
	if successVal.Exists() && !successVal.Bool() {
		errMsg := root.Get("error.errorMessage").String()
		return nil, fmt.Errorf("%w: upstream success=false: %s", ErrInvalidResponseSchema, nonEmpty(errMsg, "unknown error"))
	}

	arr := root.Get("result.hmeEmails")
	if !arr.Exists() || !arr.IsArray() {
		return nil, fmt.Errorf("%w: missing or invalid result.hmeEmails array in response", ErrInvalidResponseSchema)
	}

	var aliases []Alias
	arr.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		meta := item.Get("metaData")
		email := strings.TrimSpace(strings.ToLower(firstNonEmpty(
			item.Get("hme").String(),
			item.Get("email").String(),
			item.Get("alias").String(),
			item.Get("address").String(),
			meta.Get("hme").String(),
		)))
		if email == "" || !strings.Contains(email, "@") {
			return true
		}
		state := strings.ToLower(firstNonEmpty(item.Get("state").String(), item.Get("status").String()))
		active := state != "inactive" && state != "deleted"
		if item.Get("active").Exists() {
			active = item.Get("active").Bool() && active
		}
		if item.Get("isActive").Exists() {
			active = item.Get("isActive").Bool() && active
		}
		created := formatTimestamp(item.Get("createTimestamp"))
		if created == "" {
			created = formatTimestamp(item.Get("createdAt"))
		}
		aliases = append(aliases, Alias{
			Email:       email,
			AnonymousID: firstNonEmpty(item.Get("anonymousId").String(), item.Get("id").String()),
			Label:       firstNonEmpty(item.Get("label").String(), meta.Get("label").String()),
			Active:      active,
			CreatedAt:   created,
		})
		return true
	})

	if aliases == nil {
		aliases = []Alias{}
	}

	// 活跃的排前面,再按邮箱字母序。
	sort.SliceStable(aliases, func(i, j int) bool {
		if aliases[i].Active != aliases[j].Active {
			return aliases[i].Active
		}
		return aliases[i].Email < aliases[j].Email
	})
	return aliases, nil
}

// formatTimestamp 将 iCloud 返回的各类时间戳转换为标准 RFC3339 字符串。
// 兼容秒级、毫秒级、微秒级、纳秒级数值/数值字符串，以及已格式化的 RFC3339/ISO 时间字符串。
func formatTimestamp(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.Number {
		return epochToRFC3339(v.Int())
	}
	s := strings.TrimSpace(v.String())
	if s == "" {
		return ""
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return epochToRFC3339(int64(f))
	}
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

func epochToRFC3339(n int64) string {
	if n <= 0 {
		return ""
	}
	switch {
	case n < 1e11: // 秒级
		return time.Unix(n, 0).UTC().Format(time.RFC3339)
	case n < 1e14: // 毫秒级 (iCloud createTimestamp 如 1789000919081)
		return time.UnixMilli(n).UTC().Format(time.RFC3339)
	case n < 1e17: // 微秒级
		return time.UnixMicro(n).UTC().Format(time.RFC3339)
	default: // 纳秒级
		return time.Unix(0, n).UTC().Format(time.RFC3339)
	}
}
