import asyncio
import sys
from camoufox.async_api import AsyncCamoufox

async def main():
    print("=" * 60)
    print("  [Camoufox 浏览器可视化演示]")
    print("  正在拉起独立 Camoufox 窗口并访问 https://www.icloud.com.cn/ ...")
    print("=" * 60)
    async with AsyncCamoufox(
        headless=False,
        os="windows",
        humanize=True,
    ) as browser:
        context = await browser.new_context(viewport={"width": 1280, "height": 800})
        page = await context.new_page()
        print("\n[*] 正在加载云上贵州 iCloud 官网 (https://www.icloud.com.cn/)...")
        await page.goto("https://www.icloud.com.cn/", wait_until="domcontentloaded")
        print("[+] 浏览器窗口已成功在桌面上弹出！")
        print("[*] 窗口将保持打开 300 秒（5分钟），你可以自由查看与点击测试，关闭窗口或终端即可退出。")
        await asyncio.sleep(300)

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n用户主动退出")
        sys.exit(0)
