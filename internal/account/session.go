/**
 * [OUTPUT]: 会话恢复预算持久化及只读列表的一次恢复重试
 * [POS]: internal/account 的会话管理层
 */

package account

import (
	"context"
	"errors"
	"icloud-hme/internal/hme"
)

// Persist recovery limits even if authentication fails; never commit cookies
// from a rejected session. Manual credential replacement wins by epoch.
func (m *Manager) saveRecoveryProgress(id string, before *Account, session *hme.BrowserSession) error {
	if before.Session == nil || session == nil {
		return nil
	}
	if before.Session.RecoveryAfter == session.RecoveryAfter && before.Session.RecoveryBlocked == session.RecoveryBlocked {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.accounts[id]
	if !ok || cur.credentialEpoch != before.credentialEpoch || cur.Host != before.Host || cur.Proxy != before.Proxy {
		return ErrSessionChanged
	}
	old := cur.Session
	next := old.Clone()
	if next == nil {
		return ErrSessionChanged
	}
	next.RecoveryAfter = session.RecoveryAfter
	next.RecoveryBlocked = session.RecoveryBlocked
	cur.Session = next
	if err := m.saveAccount(cur); err != nil {
		cur.Session = old
		return err
	}
	return nil
}

// List reads are safe to repeat once; mutations never go through this helper.
func listAliasesForSession(ctx context.Context, client *hme.Client) ([]hme.Alias, error) {
	return listAliasesWithRecovery(
		func() ([]hme.Alias, error) { return client.ListAliasesWithContext(ctx) },
		func() error { return client.RecoverSession(ctx) }, client.SessionSnapshot() != nil)
}

func listAliasesWithRecovery(list func() ([]hme.Alias, error), recoverSession func() error, browser bool) ([]hme.Alias, error) {
	aliases, err := list()
	if errors.Is(err, hme.ErrAuthFailed) && browser {
		if recoveryErr := recoverSession(); recoveryErr != nil {
			return nil, recoveryErr
		}
		return list()
	}
	return aliases, err
}
