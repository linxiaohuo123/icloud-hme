import { describe, expect, it } from 'vitest'
import { buildCurlSnippet, buildLeaseCommand, buildPythonSnippet } from './snippets'

describe('snippets', () => {
  const origin = 'http://localhost:8081'
  const token = 'tok_test123'

  it('buildLeaseCommand encodes special characters in tag', () => {
    const cmd = buildLeaseCommand(origin, 'tiktok&v=2', token)
    expect(cmd).toContain('tag=tiktok%26v%3D2')
    expect(cmd).toContain(`Bearer ${token}`)
  })

  it('buildCurlSnippet generates complete two-step instructions with encoded tag', () => {
    const curl = buildCurlSnippet(origin, 'my tag', token)
    expect(curl).toContain('curl -X POST "http://localhost:8081/api/quick-create?tag=my%20tag"')
    expect(curl).toContain('curl "http://localhost:8081/api/verify-code?email=mysterious.tiger_0x@icloud.com&timeout=60&auto_delete=true"')
    expect(curl).toContain(`Bearer ${token}`)

    // 行连接符必须是「字面反斜杠 + 换行」两个字符。
    // 若源码里只写单个 \，JS 模板字符串会把它当行连接符连同换行一起吞掉，
    // 生成的命令会塌成一行(虽然仍可执行，但与后续示例的格式不一致)。
    const BACKSLASH = String.fromCharCode(92)
    expect(curl).toContain(`/api/quick-create?tag=my%20tag" ${BACKSLASH}\n  -H`)
  })

  it('buildPythonSnippet includes origin and token in pipeline', () => {
    const py = buildPythonSnippet(origin, 'tiktok', token)
    expect(py).toContain(`BASE_URL = "${origin}"`)
    expect(py).toContain(`API_TOKEN = "${token}"`)
    expect(py).toContain('TAG = "tiktok"')
    expect(py).toContain('requests.post')
    expect(py).toContain('requests.get')
  })
})
