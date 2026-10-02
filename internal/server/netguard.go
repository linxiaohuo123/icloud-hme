/**
 * [INPUT]: 依赖 internal/security 的共享通知出站策略
 * [OUTPUT]: validateOutboundPublicURL，将地址错误适配为通知配置错误
 * [POS]: internal/server 的通知配置校验入口，与发送端共享内网判定
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
package server

import "icloud-hme/internal/security"

const allowPrivateOutboundEnv = security.AllowPrivateOutboundEnv

func validateOutboundPublicURL(raw, label string) error {
	if err := security.ValidateOutboundPublicURL(raw); err != nil {
		return errNotify(label + " " + err.Error())
	}
	return nil
}
