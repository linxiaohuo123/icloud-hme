/**
 * [INPUT]: 依赖 utils/mail (buildMailCacheKey)
 * [OUTPUT]: 对外提供邮件引用与多租户/多文件夹缓存键隔离测试
 * [POS]: web/src/utils 的邮件工具函数单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { describe, expect, it } from 'vitest'
import { buildMailCacheKey } from './mail'

describe('buildMailCacheKey', () => {
  it('优先使用 message_ref 进行全局隔离', () => {
    const key1 = buildMailCacheKey('acc_1', { message_ref: 'ref_v1_abc' })
    const key2 = buildMailCacheKey('acc_2', { message_ref: 'ref_v1_abc' })
    expect(key1).toBe('acc_1:ref_v1_abc')
    expect(key2).toBe('acc_2:ref_v1_abc')
    expect(key1).not.toBe(key2)
  })

  it('在无 message_ref 时隔离不同文件夹下的相同 UID', () => {
    const inboxKey = buildMailCacheKey('acc_1', { folder: 'INBOX', uid: 42, uid_validity: 100 })
    const junkKey = buildMailCacheKey('acc_1', { folder: 'Junk', uid: 42, uid_validity: 100 })
    expect(inboxKey).toBe('acc_1:INBOX:100:42')
    expect(junkKey).toBe('acc_1:Junk:100:42')
    expect(inboxKey).not.toBe(junkKey)
  })

  it('在 UIDVALIDITY 改变时产生不同缓存键', () => {
    const keyV1 = buildMailCacheKey('acc_1', { folder: 'INBOX', uid: 42, uid_validity: 100 })
    const keyV2 = buildMailCacheKey('acc_1', { folder: 'INBOX', uid: 42, uid_validity: 200 })
    expect(keyV1).toBe('acc_1:INBOX:100:42')
    expect(keyV2).toBe('acc_1:INBOX:200:42')
    expect(keyV1).not.toBe(keyV2)
  })

  it('隔离不同账号下的相同 UID', () => {
    const acc1Key = buildMailCacheKey('acc_1', { folder: 'INBOX', id: '42' })
    const acc2Key = buildMailCacheKey('acc_2', { folder: 'INBOX', id: '42' })
    expect(acc1Key).toBe('acc_1:INBOX:0:42')
    expect(acc2Key).toBe('acc_2:INBOX:0:42')
    expect(acc1Key).not.toBe(acc2Key)
  })
})
