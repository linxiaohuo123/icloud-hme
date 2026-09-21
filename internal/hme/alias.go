/**
 * [INPUT]: 依赖 github.com/tidwall/gjson, time, strings, sort, strconv, fmt
 * [OUTPUT]: 对外提供 Alias, CreateResult, ListAliases, Generate, Reserve, CreateAlias, DeactivateHME, ReactivateHME, Delete, UpdateMetaData
 * [POS]: internal/hme 的 HME 别名协议操作层，负责别名生成、保留、列出、激活、修改与删除
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package hme

import (
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
	Email     string `json:"email"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
}

// ListAliases 列出当前账号所有 Hide My Email 别名。
func (c *Client) ListAliases() ([]Alias, error) {
	if err := c.resolveService(); err != nil {
		return nil, err
	}
	c.log("获取别名列表...")
	body, err := c.request("GET", c.ServiceURL()+"/v2/hme/list", nil, 0, MaxRetries)
	if err != nil {
		// 若已缓存的 serviceURL 失效，清空并重新走一次 ValidateSession 自愈
		if c.ServiceURL() != "" {
			c.ResetServiceEndpoint()
			if resolveErr := c.resolveService(); resolveErr == nil {
				body, err = c.request("GET", c.ServiceURL()+"/v2/hme/list", nil, 0, MaxRetries)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	aliases := parseAliasList(body)
	c.log("共 %d 个别名", len(aliases))
	return aliases, nil
}

// Generate 生成一个候选别名(尚未保留,需再调用 Reserve)。
func (c *Client) Generate() (string, error) {
	if err := c.resolveService(); err != nil {
		return "", err
	}
	c.log("生成候选别名...")
	body, err := c.request("POST", c.ServiceURL()+"/v1/hme/generate", map[string]string{"langCode": "en-us"}, 0, 2)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		errMsg := parsed.Get("error.errorMessage").String()
		return "", fmt.Errorf("生成失败: %s", nonEmpty(errMsg, "unknown"))
	}
	// 某些响应把 hme 包在嵌套对象里: gjson 对 object 调 .String() 返回原始 JSON 文本(非空),
	// 必须先用 IsObject 判别,否则嵌套分支永远走不到,会把 JSON 原文当别名地址传给 Reserve
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
	c.log("候选: %s", hme)
	return hme, nil
}

// Reserve 保留/确认候选别名,使其正式生效。
func (c *Client) Reserve(hme, label string) (string, error) {
	if err := c.resolveService(); err != nil {
		return "", err
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
	body, err := c.request("POST", c.ServiceURL()+"/v1/hme/reserve", payload, 0, 2)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		errMsg := parsed.Get("error.errorMessage").String()
		return "", fmt.Errorf("保留失败: %s", nonEmpty(errMsg, "unknown"))
	}
	alias := hme
	resultHme := parsed.Get("result.hme")
	if resultHme.IsObject() {
		if v := resultHme.Get("hme").String(); v != "" {
			alias = v
		}
	}
	c.log("已保留: %s", alias)
	return alias, nil
}

// CreateAlias 一步完成「生成 + 保留」,创建一个新别名。
//
// 由于 generate / reserve 偶发失败,内部会重试 maxRetries 次,
// 每次重试会重置 serviceURL 强制重新校验会话。
func (c *Client) CreateAlias(label string, maxRetries int) (*CreateResult, error) {
	if maxRetries <= 0 {
		maxRetries = 5
	}
	var lastErr string
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			c.ResetServiceEndpoint()
			c.log("重试 %d/%d ...", attempt+1, maxRetries)
		}
		hme, err := c.Generate()
		if err != nil {
			lastErr = "generate 失败: " + err.Error()
			c.log("%s", lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(time.Second)
				continue
			}
			break
		}
		email, err := c.Reserve(hme, label)
		if err != nil {
			lastErr = err.Error()
			c.log("reserve 失败: %s", lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(time.Second)
				continue
			}
			break
		}
		return &CreateResult{
			Email:     email,
			Label:     label,
			CreatedAt: time.Now().Format(time.RFC3339),
		}, nil
	}
	if lastErr != "" {
		return nil, fmt.Errorf("创建别名失败: %s", lastErr)
	}
	return nil, fmt.Errorf("创建别名失败,已重试 %d 次", maxRetries)
}

// DeactivateHME 停用别名(可恢复)。
func (c *Client) DeactivateHME(anonymousID string) (bool, error) {
	if err := c.resolveService(); err != nil {
		return false, err
	}
	c.log("停用 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.request("POST", c.ServiceURL()+"/v1/hme/deactivate", payload, 0, 2)
	if err != nil {
		return false, err
	}
	return gjson.Get(body, "success").Bool(), nil
}

// ReactivateHME 激活已停用的别名。
func (c *Client) ReactivateHME(anonymousID string) (bool, error) {
	if err := c.resolveService(); err != nil {
		return false, err
	}
	c.log("激活 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.request("POST", c.ServiceURL()+"/v1/hme/reactivate", payload, 0, 2)
	if err != nil {
		return false, err
	}
	return gjson.Get(body, "success").Bool(), nil
}

// Delete 删除别名。若直接删除失败会先停用再删。
func (c *Client) Delete(anonymousID string) error {
	if err := c.resolveService(); err != nil {
		return err
	}
	c.log("删除 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	doDelete := func() (string, error) {
		return c.request("POST", c.ServiceURL()+"/v1/hme/delete", payload, 0, 2)
	}
	body, err := doDelete()
	if err != nil || !gjson.Get(body, "success").Bool() {
		c.log("直接删除失败,尝试先停用...")
		_, _ = c.request("POST", c.ServiceURL()+"/v1/hme/deactivate", payload, 0, 2)
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

// UpdateMetaData 更新别名备注 (label) 与说明 (note)。
func (c *Client) UpdateMetaData(anonymousID, label, note string) error {
	if err := c.resolveService(); err != nil {
		return err
	}
	c.log("更新别名备注 %s -> %s ...", anonymousID, label)
	payload := map[string]string{
		"anonymousId": anonymousID,
		"label":       label,
		"note":        note,
	}
	body, err := c.request("POST", c.ServiceURL()+"/v1/hme/updateMetaData", payload, 0, 2)
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

// ---- 别名列表解析 (对应 ICloudHME._parse_alias_list) ----

// parseAliasList 解析 iCloud 返回的别名列表 JSON。
// 容错:优先取 result.hmeEmails,找不到则递归查找第一个对象数组。
func parseAliasList(body string) []Alias {
	if !gjson.Valid(body) {
		return []Alias{}
	}
	root := gjson.Parse(body)

	arr := root.Get("result.hmeEmails")
	if !arr.IsArray() {
		arr = findFirstDictArray(root)
	}
	if !arr.IsArray() {
		return []Alias{}
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

	// 活跃的排前面,再按邮箱字母序。
	sort.SliceStable(aliases, func(i, j int) bool {
		if aliases[i].Active != aliases[j].Active {
			return aliases[i].Active
		}
		return aliases[i].Email < aliases[j].Email
	})
	return aliases
}

// findFirstDictArray 递归查找第一个「对象数组」。
func findFirstDictArray(v gjson.Result) gjson.Result {
	if v.IsArray() {
		if len(v.Array()) > 0 && v.Array()[0].IsObject() {
			return v
		}
	}
	if v.IsObject() {
		var found gjson.Result
		v.ForEach(func(_, val gjson.Result) bool {
			if r := findFirstDictArray(val); r.IsArray() && len(r.Array()) > 0 {
				found = r
				return false
			}
			return true
		})
		return found
	}
	return gjson.Result{}
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
