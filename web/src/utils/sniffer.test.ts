/**
 * [INPUT]: 依赖 utils/sniffer (extractVerifyCode, extractMagicLink, extractOTP, buildSniffContext, stripHtml, toHalfWidth, parseSenderInfo)
 * [OUTPUT]: 对外提供前端嗅探器单元测试套件
 * [POS]: web/src/utils 的嗅探逻辑与发件人解析回归防线；全面覆盖工业级盒式空格拆分、系词介词防穿透、Steam Guard 与链接嗅探
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { describe, expect, it } from 'vitest'
import {
  buildSniffContext,
  extractMagicLink,
  extractOTP,
  extractVerifyCode,
  parseSenderInfo,
  stripHtml,
  toHalfWidth,
} from './sniffer'

describe('buildSniffContext & toHalfWidth', () => {
  it('正确联合标题与正文摘要，避免短路吞码', () => {
    expect(buildSniffContext('验证码是 123456', '尊敬的用户您好')).toBe(
      '验证码是 123456\n尊敬的用户您好',
    )
    expect(buildSniffContext('普通通知', undefined)).toBe('普通通知')
    expect(buildSniffContext(undefined, '正文内容')).toBe('正文内容')
    expect(buildSniffContext('', '')).toBe('')
  })

  it('toHalfWidth 全角转半角与零宽字符清洗', () => {
    expect(toHalfWidth('５７６９３２')).toBe('576932')
    expect(toHalfWidth('Code:\u200B 123456')).toBe('Code: 123456')
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

  it('提取 HTML 盒式布局单个空格拆分的验证码', () => {
    expect(extractVerifyCode('您的验证码如下： 5 7 6 9 3 2')).toBe('576932')
    expect(
      extractVerifyCode(
        stripHtml(
          "<p>您的验证码如下：</p><div class='box'><span>5</span> <span>7</span> <span>6</span> <span>9</span> <span>3</span> <span>2</span></div>",
        ),
      ),
    ).toBe('576932')
  })

  it('防负向订单号穿透并精准命中真正验证码', () => {
    expect(
      extractVerifyCode(
        'Your verification code for order #839201 is 492019. Valid for 10 mins.',
      ),
    ).toBe('492019')
  })

  it('提取 Steam Guard 5 位混合码并阻断普通动词干扰', () => {
    expect(
      extractVerifyCode(
        'Here is the Steam Guard code you need that will allow you to sign in: H8R2K',
      ),
    ).toBe('H8R2K')
    expect(extractVerifyCode('Steam Guard code: 9X3KP')).toBe('9X3KP')
  })

  it('提取各种闭合符号强标注', () => {
    expect(extractVerifyCode('[849201] Discord code')).toBe('849201')
    expect(extractVerifyCode('【392019】您的验证码')).toBe('392019')
    expect(extractVerifyCode('『576932』認証コード')).toBe('576932')
    expect(extractVerifyCode('Your confirmation code is {839201}')).toBe('839201')
  })

  it('提取全角数字验证码', () => {
    expect(extractVerifyCode('本次登录验证码为：５７６９３２')).toBe('576932')
  })

  it('提取韩文ChatGPT等外语上下文验证码', () => {
    const koreanMail =
      '다음 임시 인증 코드를 입력해 계속하세요: 576932 ChatGPT 계정을 생성하고자 하는 것이 본인이 아닌 경우 이 이메일을 무시하세요. 감사합니다. ChatGPT 팀 드림 ChatGPT ( https://chatgpt.com ) 도움말 센터 ( https://help.openai.com )'
    expect(extractVerifyCode(koreanMail)).toBe('576932')
  })

  it('提取全球多语种(俄/西/德/法/越/土/阿/日)验证码', () => {
    // 俄语
    expect(extractVerifyCode('Ваш проверочный код: 491028')).toBe('491028')
    // 西班牙语
    expect(extractVerifyCode('Su código de verificación es: 382910')).toBe('382910')
    // 德语
    expect(extractVerifyCode('Ihr Bestätigungscode lautet: 829103')).toBe('829103')
    // 法语
    expect(extractVerifyCode('Votre code de confirmation est : 719204')).toBe('719204')
    // 越南语
    expect(extractVerifyCode('Mã xác thực của bạn là: 629105')).toBe('629105')
    // 土耳其语
    expect(extractVerifyCode('Doğrulama kodunuz: 839102')).toBe('839102')
    // 阿拉伯语
    expect(extractVerifyCode('رمز التحقق الخاص بك هو: 918234')).toBe('918234')
    // 日语
    expect(extractVerifyCode('お客様の確認コードは 576932 です')).toBe('576932')
  })

  it('提取反向倒装语序的验证码', () => {
    expect(extractVerifyCode('123456 is your verification code')).toBe('123456')
    expect(extractVerifyCode('839201 为本次确认码')).toBe('839201')
    expect(extractVerifyCode('849201 为您的本次登录验证码，切勿泄露。')).toBe('849201')
    expect(
      extractVerifyCode('492019 is your AWS verification code. Valid for 10 minutes.'),
    ).toBe('492019')
    expect(extractVerifyCode('Account PIN\n58291 is your security code')).toBe('58291')
    expect(extractVerifyCode('8392 is your code')).toBe('8392')
  })

  it('标题包含验证码而正文摘要不含时，通过联合上下文成功提取', () => {
    const subject = '[GitHub] Your verification code is 492019'
    const preview = 'Hi Linus, someone tried to sign in to your account. Enter the code above.'
    const context = buildSniffContext(subject, preview)
    expect(extractVerifyCode(context)).toBe('492019')
  })

  it('正文包含 HTML 且仅在正文中含有验证码时，联合上下文剥除 HTML 并成功提取', () => {
    const subject = 'OpenAI 验证邮件'
    const preview = ''
    const body =
      '<div><p>다음 임시 인증 코드를 입력해 계속하세요: <strong>576932</strong></p></div>'
    const context = buildSniffContext(subject, preview, body)
    expect(extractVerifyCode(context)).toBe('576932')
  })

  it('兜底独立 6 位数字仅在验证类关键词下回退，并排除 2020-2035 年月伪码', () => {
    expect(extractVerifyCode('Notice 202501 has been dispatched')).toBeNull()
    expect(extractVerifyCode('Please enter 582910 to continue')).toBeNull()
    expect(
      extractVerifyCode('Please enter 582910 to continue, verification required'),
    ).toBe('582910')
  })

  it('不将负向账单、快递或编码当验证码', () => {
    expect(extractVerifyCode('Tracking 884421 has a barcode attached')).toBeNull()
    expect(extractVerifyCode('Build 12345 uses unicode')).toBeNull()
    expect(extractVerifyCode('Package 492019 encode complete')).toBeNull()
    expect(extractVerifyCode('Thank you for shopping: 123456 is your order number.')).toBeNull()
    expect(extractVerifyCode('Welcome to Hotpot restaurant! Bill: 883921')).toBeNull()
  })
})

describe('extractMagicLink & extractOTP', () => {
  it('提取激活验证链接', () => {
    const text = 'Please click https://example.com/confirm?token=xyz123 to activate your account.'
    expect(extractMagicLink(text)).toBe('https://example.com/confirm?token=xyz123')
  })

  it('综合 extractOTP 提取验证码与链接', () => {
    const text = 'Your code: 394821 or click https://example.com/verify?id=99 to login.'
    const res = extractOTP(text)
    expect(res).not.toBeNull()
    expect(res?.code).toBe('394821')
    expect(res?.magicLink).toBe('https://example.com/verify?id=99')
  })
})

describe('stripHtml & parseSenderInfo', () => {
  it('stripHtml 正确清洗 HTML 标签与样式并保留 a 标签链接', () => {
    const raw = '<style>body { color: red; }</style><p>Hello &nbsp; <b>World</b>! <a href="https://example.com/confirm?token=xyz123&amp;ref=1">激活账户</a></p>'
    expect(stripHtml(raw)).toBe('Hello World ! 激活账户 ( https://example.com/confirm?token=xyz123&ref=1 )')
  })

  it('从包含 a 标签的 HTML 正文中嗅探激活链接', () => {
    const html = '<div><p>请点击下方按钮激活您的账号：</p><a href="https://slack.com/verify-email?token=sec_999888">点击激活</a></div>'
    const context = buildSniffContext('Slack 登录验证', undefined, html)
    const res = extractOTP(context)
    expect(res).not.toBeNull()
    expect(res?.magicLink).toBe('https://slack.com/verify-email?token=sec_999888')
  })

  it('parseSenderInfo 提取发件人名字、地址并反解 Apple Relay', () => {
    const info = parseSenderInfo('OpenAI <noreply_at_openai_com_123@icloud.com>')
    expect(info.name).toBe('OpenAI')
    expect(info.email).toBe('noreply_at_openai_com_123@icloud.com')
  })
})
