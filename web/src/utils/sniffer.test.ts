/**
 * [INPUT]: 依赖 utils/sniffer (extractVerifyCode, buildSniffContext, stripHtml, parseSenderInfo)
 * [OUTPUT]: 对外提供前端嗅探器单元测试套件
 * [POS]: web/src/utils 的嗅探逻辑与发件人解析回归防线
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { describe, expect, it } from 'vitest'
import {
  buildSniffContext,
  extractVerifyCode,
  parseSenderInfo,
  stripHtml,
} from './sniffer'

describe('buildSniffContext', () => {
  it('正确联合标题与正文摘要，避免短路吞码', () => {
    expect(buildSniffContext('验证码是 123456', '尊敬的用户您好')).toBe(
      '验证码是 123456 尊敬的用户您好',
    )
    expect(buildSniffContext('普通通知', undefined)).toBe('普通通知')
    expect(buildSniffContext(undefined, '正文内容')).toBe('正文内容')
    expect(buildSniffContext('', '')).toBe('')
  })
})

describe('extractVerifyCode', () => {
  it('提取正向中文上下文验证码', () => {
    expect(extractVerifyCode('您的验证码是：849201，请在 10 分钟内输入。')).toBe('849201')
    expect(extractVerifyCode('本次登录动态码: 1234')).toBe('1234')
    expect(extractVerifyCode('安全码为 98765432')).toBe('98765432')
  })

  it('提取正向英文上下文验证码', () => {
    expect(extractVerifyCode('Your verification code is 394821')).toBe('394821')
    expect(extractVerifyCode('Your code: 582910. Do not share.')).toBe('582910')
    expect(extractVerifyCode('Security PIN is 9283')).toBe('9283')
  })

  it('提取带连字号或空格的验证码', () => {
    expect(extractVerifyCode('Your login code is 123-456')).toBe('123456')
    expect(extractVerifyCode('Your one-time password is: 987 654')).toBe('987654')
  })

  it('提取反向倒装语序的验证码', () => {
    expect(extractVerifyCode('123456 is your verification code')).toBe('123456')
    expect(extractVerifyCode('839201 为本次确认码')).toBe('839201')
    // 夹带修饰定语
    expect(extractVerifyCode('849201 为您的本次登录验证码，切勿泄露。')).toBe('849201')
    expect(extractVerifyCode('492019 is your AWS verification code. Valid for 10 minutes.')).toBe('492019')
    // 5 位倒装码 (测试非 6 位倒装不漏检)
    expect(extractVerifyCode('49201 is your security code')).toBe('49201')
    // 4 位倒装码
    expect(extractVerifyCode('8392 is your code')).toBe('8392')
  })

  it('标题包含验证码而正文摘要不含时，通过联合上下文成功提取', () => {
    const subject = '[GitHub] Your verification code is 492019'
    const preview = 'Hi Linus, someone tried to sign in to your account. Enter the code above.'
    const context = buildSniffContext(subject, preview)
    expect(extractVerifyCode(context)).toBe('492019')
  })

  it('兜底独立 6 位数字仅在验证类关键词下回退，并排除 2020-2035 年月伪码', () => {
    expect(extractVerifyCode('Notice 202501 has been dispatched')).toBeNull()
    expect(extractVerifyCode('Please enter 582910 to continue')).toBeNull()
    expect(extractVerifyCode('Please enter 582910 to continue, verification required')).toBe('582910')
  })

  it('反向正则不把 barcode/unicode/encode 当验证码', () => {
    expect(extractVerifyCode('Tracking 884421 has a barcode attached')).toBeNull()
    expect(extractVerifyCode('Build 12345 uses unicode')).toBeNull()
    expect(extractVerifyCode('Package 492019 encode complete')).toBeNull()
  })
})

describe('stripHtml & parseSenderInfo', () => {
  it('stripHtml 正确清洗 HTML 标签与样式', () => {
    const raw = '<style>body { color: red; }</style><p>Hello &nbsp; <b>World</b>!</p>'
    expect(stripHtml(raw)).toBe('Hello World !')
  })

  it('parseSenderInfo 提取发件人名字、地址并反解 Apple Relay', () => {
    const info = parseSenderInfo('OpenAI <noreply_at_openai_com_123@icloud.com>')
    expect(info.name).toBe('OpenAI')
    expect(info.email).toBe('noreply_at_openai_com_123@icloud.com')
  })
})
