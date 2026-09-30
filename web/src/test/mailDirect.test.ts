/// <reference types="node" />

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mailScript = readFileSync(resolve(process.cwd(), '../internal/server/mail_direct.js'), 'utf8')

function item(index = 0) {
  return {
    index, id: `mail-${index}`, subject: 'Sign in', sender_name: 'Fixture',
    sender_initial: 'F', relative_date: '刚刚', text_body: 'Loading preview',
    html_body: '', is_html: false, has_otp: false, code: '', magic_link: '',
    body_error: '', body_complete: true,
  }
}

type MailItem = ReturnType<typeof item>
let removeListeners = () => {}

function boot(items: MailItem[]) {
  document.body.innerHTML = `
    <div id="mailViewData"></div>
    <div id="mailListContainer">${items.map(it => `<div class="mail-item" id="mailItem-${it.index}" data-action="select-mail" data-index="${it.index}">Mail</div>`).join('')}</div>
    <div id="otpMasterBar" class="is-hidden"><span id="topLatestOtp"></span><a id="topMagicBtn" class="is-hidden"></a></div>
    <div id="detailOtpBox" class="is-hidden"><span id="detailOtpCode"></span><a id="detailMagicBtn" class="is-hidden"></a></div>
    <p id="bodyStatus" class="is-hidden"></p><div id="rawContentArea"></div><div id="styledContentArea"></div>
    <button class="tab-btn" id="tab-styled" data-action="switch-tab" data-view="styled">正文</button>
    <button class="tab-btn" id="tab-raw" data-action="switch-tab" data-view="raw">原文</button>
    <div class="body-view" id="view-styled"></div><div class="body-view" id="view-raw"></div>
    <button id="refresh" data-action="manual-refresh">刷新</button>`
  document.getElementById('mailViewData')!.dataset.items = JSON.stringify(items)
  const registration = vi.spyOn(document, 'addEventListener')
  window.eval(`(function () { ${mailScript}\n})()`)
  const listeners = registration.mock.calls.slice()
  registration.mockRestore()
  removeListeners = () => listeners.forEach(([type, listener, options]) => document.removeEventListener(type, listener, options))
}

function respond(items: MailItem[]) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ success: true, data: {
      email: 'fixture@icloud.com', has_mail: items.length > 0, total_count: items.length,
      items, all_otps: items.filter(it => it.has_otp && (it.code || it.magic_link)),
    } }),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  removeListeners()
  vi.clearAllTimers()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('direct mail polling', () => {

  it('shows body read errors and clears them after a successful refresh', async () => {
    boot([{ ...item(), body_error: 'broken MIME', body_complete: false, text_body: '' }])
    expect(document.getElementById('bodyStatus')).toHaveTextContent('正文读取失败：broken MIME')
    expect(document.getElementById('bodyStatus')).not.toHaveClass('is-hidden')
    respond([item()])
    await vi.advanceTimersByTimeAsync(3000)
    expect(document.getElementById('bodyStatus')).toHaveClass('is-hidden')
    expect(document.getElementById('rawContentArea')).toHaveTextContent('Loading preview')
  })

  it('marks incomplete WebMail bodies as previews', () => {
    boot([{ ...item(), body_complete: false }])
    expect(document.getElementById('bodyStatus')).toHaveTextContent('非完整正文')
    expect(document.getElementById('bodyStatus')).not.toHaveClass('is-hidden')
  })
  it.each(['automatic', 'manual'])('updates same-message magic link and body on %s refresh', async mode => {
    const initial = item()
    boot([initial])
    const updated = { ...initial, has_otp: true, magic_link: 'https://example.com/verify?token=updated', text_body: 'Updated activation link' }
    const fetchMock = respond([updated])
    if (mode === 'manual') document.getElementById('refresh')!.click()
    await vi.advanceTimersByTimeAsync(mode === 'automatic' ? 3000 : 0)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(document.getElementById('detailMagicBtn')).not.toHaveClass('is-hidden')
    expect(document.getElementById('detailMagicBtn')).toHaveAttribute('href', updated.magic_link)
    expect(document.getElementById('rawContentArea')).toHaveTextContent(updated.text_body)
    expect(document.getElementById('topMagicBtn')).toHaveAttribute('href', updated.magic_link)
  })

  it('keeps the selected older mail and reading tab when its body changes', async () => {
    const items = [item(0), item(1)]
    boot(items)
    document.getElementById('mailItem-1')!.click()
    document.getElementById('tab-raw')!.click()
    respond([items[0], { ...items[1], text_body: 'Older mail full body', is_html: true }])
    await vi.advanceTimersByTimeAsync(3000)
    expect(document.getElementById('mailItem-1')).toHaveClass('active')
    expect(document.getElementById('mailItem-0')).not.toHaveClass('active')
    expect(document.getElementById('tab-raw')).toHaveClass('active')
    expect(document.getElementById('rawContentArea')).toHaveTextContent('Older mail full body')
  })

  it('removes a stale detail magic link when verification data changes', async () => {
    boot([{ ...item(), has_otp: true, magic_link: 'https://example.com/verify?token=old' }])
    respond([item()])
    await vi.advanceTimersByTimeAsync(3000)
    expect(document.getElementById('detailMagicBtn')).toHaveClass('is-hidden')
    expect(document.getElementById('detailMagicBtn')).not.toHaveAttribute('href')
  })

  it('selects a newly arrived mail as before', async () => {
    boot([item()])
    respond([{ ...item(), id: 'new-mail', text_body: 'New mail body' }, { ...item(1), id: 'mail-0' }])
    await vi.advanceTimersByTimeAsync(3000)
    expect(document.getElementById('mailItem-0')).toHaveClass('active')
    expect(document.getElementById('rawContentArea')).toHaveTextContent('New mail body')
  })
})
