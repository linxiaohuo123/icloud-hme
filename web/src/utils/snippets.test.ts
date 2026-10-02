import { describe, expect, it } from 'vitest'
import { buildCurlSnippet, buildLeaseCommand, buildPythonSnippet } from './snippets'

describe('snippets', () => {
  const origin = 'http://localhost:8081'
  const token = 'tok_test123'

  it('buildLeaseCommand uses v2 allocate with idempotency key and shell-safe tag body', () => {
    const cmd = buildLeaseCommand(origin, "it's&v=2", token)
    expect(cmd).toContain('curl -X POST "http://localhost:8081/api/external/v2/allocate"')
    expect(cmd).toContain('Idempotency-Key: ')
    expect(cmd).toContain(`-d '{"tag":"it'\\''s&v=2"}'`)
    expect(cmd).toContain(`Bearer ${token}`)
  })

  it('buildCurlSnippet generates the v2 allocate, verification request and long-poll steps', () => {
    const curl = buildCurlSnippet(origin, 'my tag', token)
    expect(curl).toContain('"http://localhost:8081/api/external/v2/allocate"')
    expect(curl).toContain('"http://localhost:8081/api/external/v2/verification-requests"')
    expect(curl).toContain('/api/external/v2/verification-requests/vreq_8a3d1e4f?timeout=60')
    expect(curl).toContain(`-d '{"tag":"my tag"}'`)
    expect(curl).not.toContain('/api/quick-create')
    expect(curl).not.toContain('/api/verify-code')
    expect(curl).toContain(`Bearer ${token}`)

    // 行连接符必须是「字面反斜杠 + 换行」两个字符。
    // 若源码里只写单个 \，JS 模板字符串会把它当行连接符连同换行一起吞掉，
    // 生成的命令会塌成一行(虽然仍可执行，但与后续示例的格式不一致)。
    const BACKSLASH = String.fromCharCode(92)
    expect(curl).toContain(`/api/external/v2/allocate" ${BACKSLASH}\n  -H`)
  })

  it('buildPythonSnippet includes origin and token in pipeline', () => {
    const py = buildPythonSnippet(origin, 'tiktok', token)
    expect(py).toContain(`BASE_URL = "${origin}"`)
    expect(py).toContain(`API_TOKEN = "${token}"`)
    expect(py).toContain('TAG = "tiktok"')
    expect(py).toContain('/api/external/v2/allocate')
    expect(py).toContain('/api/external/v2/verification-requests')
    expect(py).toContain('Idempotency-Key')
    expect(py).not.toContain('/api/verify-code')
  })
})
