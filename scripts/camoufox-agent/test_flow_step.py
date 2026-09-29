import asyncio
import sys
from camoufox.async_api import AsyncCamoufox

async def main():
    print("=" * 60)
    print("  [Camoufox 有头流程分步排查探测器]")
    print("  目标：观察 iCloud 首页加载、点击登录按钮及 iframe 弹窗")
    print("=" * 60)

    async with AsyncCamoufox(
        headless=False,
        os="windows",
        humanize=True,
    ) as browser:
        context = await browser.new_context(viewport={"width": 1280, "height": 800})
        page = await context.new_page()

        print("\n[步骤 1/4] 访问云上贵州 iCloud 官网 (https://www.icloud.com.cn/)...")
        await page.goto("https://www.icloud.com.cn/", wait_until="domcontentloaded")
        print("[+] 页面 DOM 加载完毕，等待 2 秒稳定...")
        await asyncio.sleep(2)

        print("\n[步骤 2/4] 寻找主页【登录】按钮...")
        # 探测多种登录按钮选择器
        sign_in_btn = page.locator('ui-button:has-text("登录"), button:has-text("登录"), button:has-text("Sign In"), .sign-in-button, [data-test="sign-in-button"]')
        count = await sign_in_btn.count()
        print(f"[*] 找到符合条件的登录按钮候选数量: {count}")

        if count > 0:
            btn = sign_in_btn.first
            try:
                # 高亮按钮方便观察
                await btn.evaluate("el => el.style.border = '3px solid #00ff00'")
                print("[*] 已在浏览器中用绿色边框高亮【登录】按钮")
                await asyncio.sleep(1)
                print("[*] 正在触发点击【登录】按钮...")
                await btn.click()
                print("[+] 点击成功！")
            except Exception as e:
                print(f"[-] 直接点击异常: {e}，尝试 JS 触发...")
                await page.evaluate("""() => {
                    const btn = document.querySelector('button, ui-button');
                    for (const el of document.querySelectorAll('*')) {
                        if (el.innerText && el.innerText.trim() === '登录') {
                            el.click();
                            return;
                        }
                    }
                }""")
        else:
            print("[-] 未直接匹配到选择器，尝试全页面文本搜索...")
            await page.evaluate("""() => {
                for (const el of document.querySelectorAll('*')) {
                    if (el.children.length === 0 && el.innerText && el.innerText.trim() === '登录') {
                        el.style.border = '3px solid #ff0000';
                        el.click();
                        break;
                    }
                }
            }""")

        print("\n[步骤 3/4] 监控 10 秒内页面 iframe 变化与认证窗口...")
        for i in range(10):
            await asyncio.sleep(1)
            frames = page.frames
            print(f"  [{i+1}s] 当前页面共有 {len(frames)} 个 Frame:")
            for idx, f in enumerate(frames):
                print(f"    - Frame #{idx}: name='{f.name}', url='{f.url[:80]}'")

            # 检查是否有 aid-auth-widget
            auth_frame = None
            for f in frames:
                if "aid-auth-widget" in f.name or "idmsa.apple.com" in f.url or "appleid.apple.com" in f.url:
                    auth_frame = f
                    break
            if auth_frame:
                print(f"\n[+] 成功捕获官方认证 Frame: name='{auth_frame.name}', url='{auth_frame.url}'")
                print("\n[步骤 4/4] 探测 Frame 内的账号密码输入框...")
                try:
                    await auth_frame.wait_for_selector('input', timeout=5000)
                    inputs = await auth_frame.locator('input').all()
                    print(f"[+] Frame 内部找到 {len(inputs)} 个 input 元素:")
                    for inp in inputs:
                        itype = await inp.get_attribute("type")
                        iid = await inp.get_attribute("id")
                        iplaceholder = await inp.get_attribute("placeholder")
                        print(f"    - input: type='{itype}', id='{iid}', placeholder='{iplaceholder}'")
                except Exception as ex:
                    print(f"[-] 探测 frame 内输入框超时或异常: {ex}")
                break

        print("\n" + "=" * 60)
        print("  探测流程完毕！浏览器窗口将保持打开 180 秒供你查看排查")
        print("  你可以直接在浏览器窗口中手工操作体验，随时按 Ctrl+C 退出")
        print("=" * 60)
        await asyncio.sleep(180)

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n退出")
