import asyncio
import logging
import os
import secrets
import sys
import uuid
from contextlib import asynccontextmanager, suppress
from typing import Dict, Optional
from urllib.parse import unquote, urlsplit, urlunsplit
from browserforge.fingerprints import Screen
from fastapi import Depends, FastAPI, Header, HTTPException
from pydantic import BaseModel, Field

# 尝试导入 Camoufox
try:
    from camoufox.async_api import AsyncCamoufox
except ImportError:
    AsyncCamoufox = None

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("camoufox-agent")


def _safe_login_error(exc: BaseException) -> str:
    """将浏览器内部错误映射为不含页面原文或凭据的稳定提示。"""
    message = str(exc).lower()
    if "代理地址格式无效" in message or "代理协议不支持认证" in message:
        return "Camoufox 代理配置无效"
    if "proxy" in message.lower() or "代理连接" in message:
        return "Camoufox 代理连接失败"
    if any(marker in message for marker in ("账号或密码错误", "incorrect", "not valid", "check the account", "密码错误")):
        return "Apple ID 账号或密码错误"
    if "验证码输入失败" in message:
        return "Apple ID 双重认证验证码输入失败"
    if any(marker in message for marker in ("验证码错误", "verification code", "two-factor", "双重认证")):
        return "Apple ID 双重认证验证码无效"
    if "timeout" in message or "超时" in message:
        return "Apple ID 登录任务超时"
    return "Apple ID 登录失败，请稍后重试"

def ensure_camoufox_browser() -> bool:
    """启动前自动检查并静默拉取 Camoufox 浏览器内核，免去手动安装。"""
    try:
        from camoufox.pkgman import installed_verstr, CamoufoxNotInstalled, CamoufoxFetcher
        try:
            ver = installed_verstr()
            logger.info(f"[*] Camoufox 浏览器内核已就绪 (v{ver})")
            return True
        except CamoufoxNotInstalled:
            logger.info("[*] Camoufox 浏览器内核未就绪，正在自动拉取 (约 100MB)...")
            CamoufoxFetcher().install()
            logger.info("[+] Camoufox 浏览器内核下载并解压就绪")
            return True
    except Exception as e:
        logger.warning("[-] 自动检测 Camoufox 内核异常 (%s)", type(e).__name__)
        return False

# 服务初始化时尝试确保内核
camoufox_ready = AsyncCamoufox is not None and ensure_camoufox_browser()

@asynccontextmanager
async def lifespan(_app: FastAPI):
    if camoufox_ready:
        await refresh_camoufox_runtime_ready()
    probe_task = asyncio.create_task(refresh_camoufox_runtime_periodically())
    try:
        yield
    finally:
        probe_task.cancel()
        with suppress(asyncio.CancelledError):
            await probe_task


app = FastAPI(
    title="iCloud HME Camoufox Authentication Agent",
    description="基于 Camoufox 逆向浏览器环境的自动化 Apple ID 授权登录与 Cookie 提取微服务",
    version="1.0.0",
    lifespan=lifespan,
)

class LoginRequest(BaseModel):
    account_id: str = Field(..., description="icloud-hme 系统的账号 ID")
    username: str = Field(..., description="Apple ID 邮箱")
    password: str = Field(..., description="Apple ID 密码")
    proxy: Optional[str] = Field(None, description="住宅代理地址 (例如: http://user:pass@host:port)")
    host: Optional[str] = Field("icloud.com", description="主机: icloud.com 或 icloud.com.cn")

class SubmitOTPRequest(BaseModel):
    task_id: str = Field(..., description="登录任务 ID")
    otp_code: str = Field(..., description="6位数字双重认证验证码", min_length=6, max_length=6, pattern=r"^[0-9]{6}$")

class TaskState:
    def __init__(self, task_id: str, req: LoginRequest):
        self.task_id = task_id
        self.req = req
        self.status = "initializing"  # initializing, entering_credentials, otp_required, verifying, success, failed
        self.error_message: Optional[str] = None
        self.cookies: Optional[Dict[str, str]] = None
        self.otp_queue: asyncio.Queue = asyncio.Queue(maxsize=1)
        self.worker: Optional[asyncio.Task] = None

tasks: Dict[str, TaskState] = {}
active_tasks: set[str] = set()
MAX_ACTIVE_TASKS = 2
MAX_TASK_SECONDS = 480
TASK_RESULT_SECONDS = 120
health_probe_result = False
HEALTH_PROBE_INTERVAL_SECONDS = 120
HEALTH_PROBE_RETRY_SECONDS = 15
HEALTH_PROBE_TIMEOUT_SECONDS = 15

def require_token(x_camoufox_token: Optional[str] = Header(None)) -> None:
    expected = os.environ.get("ICLOUD_HME_CAMOUFOX_TOKEN", "")
    if not expected:
        raise HTTPException(status_code=503, detail="Camoufox 通信令牌未配置")
    if not x_camoufox_token or not secrets.compare_digest(x_camoufox_token, expected):
        raise HTTPException(status_code=401, detail="Camoufox 通信令牌无效")

async def js_click_by_text(frame, *texts: str) -> bool:
    """使用 TreeWalker 穿透 DOM 树，按文本定位并点击按钮，提升对页面结构微调的兼容性。"""
    script = """
    (targetTexts) => {
        try {
            const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT, null);
            let node;
            while (node = walker.nextNode()) {
                const val = (node.textContent || '').trim();
                for (const t of targetTexts) {
                    if (val === t || val.toLowerCase() === t.toLowerCase()) {
                        let el = node.parentElement;
                        while (el && el !== document.body) {
                            if (el.tagName === 'BUTTON' || el.tagName === 'A' || el.getAttribute('role') === 'button' || el.onclick) {
                                el.click();
                                return true;
                            }
                            el = el.parentElement;
                        }
                        if (node.parentElement && node.parentElement.offsetParent !== null) {
                            node.parentElement.click();
                            return true;
                        }
                    }
                }
            }
        } catch (e) {}
        return false;
    }
    """
    try:
        res = await frame.evaluate(script, list(texts))
        return bool(res)
    except Exception:
        return False

async def check_login_success_and_sync(task: TaskState, context) -> bool:
    """检查浏览器 Cookies 是否已包含核心凭据。"""
    try:
        raw_cookies = await context.cookies()
        cookies_map = {}
        for c in raw_cookies:
            name = c.get("name")
            val = c.get("value")
            if name and val is not None:
                val_str = str(val).strip()
                # 针对 X-APPLE-WEBAUTH-TOKEN 等关键字段，若带最外层双引号则进行标准化剥离，确保符合 RFC 规范
                if len(val_str) >= 2 and val_str.startswith('"') and val_str.endswith('"') and name in ("X-APPLE-WEBAUTH-TOKEN", "X-APPLE-DS-WEB-SESSION-TOKEN"):
                    val_str = val_str[1:-1]
                cookies_map[name] = val_str

        # 真正代表认证最终成功的核心凭据
        has_trust = "X-APPLE-WEBAUTH-HSA-TRUST" in cookies_map
        has_token = "X-APPLE-WEBAUTH-TOKEN" in cookies_map
        has_web_id = "X-APPLE-WEB-ID" in cookies_map
        # USER 可能在 2FA 完成前出现；最终仍由主服务验证 Apple 会话。
        is_success = has_token and (has_trust or has_web_id)
        if is_success:
            task.cookies = cookies_map
            task.status = "success"
            logger.info(f"[{task.task_id}] 登录成功！共提取到 {len(cookies_map)} 个 Cookie (含核心凭据: trust={has_trust}, token={has_token}, web_id={has_web_id})")
            return True
    except Exception as e:
        logger.debug("[%s] Cookie 检查异常 (%s)", task.task_id, type(e).__name__)
    return False

OTP_INPUT_SELECTOR = 'input[id^="char"], input[id^="digit"], input[autocomplete="one-time-code"], input[type="tel"], input.security-code'


async def visible_otp_inputs(page):
    for frame in page.frames:
        digits = frame.locator(OTP_INPUT_SELECTOR)
        if await digits.count() > 0 and await digits.first.is_visible():
            return digits
    return None


async def handle_redirect_and_trust(page, context, task: TaskState) -> bool:
    """处理中国区跳转 (icloud.com.cn) 与受信任浏览器弹窗，提取 Cookie"""
    if await visible_otp_inputs(page) is not None:
        return False
    if await check_login_success_and_sync(task, context):
        return True

    # 1. 检查是否存在中国区重定向提示 (Good evening. To sign in with this Apple Account, go to iCloud.com.cn.)
    redirected = False
    try:
        redirect_btn = page.locator('a:has-text("iCloud.com.cn"), button:has-text("iCloud.com.cn"), .domain-redirect-page a, [data-test="domain-redirect-button"]')
        if await redirect_btn.count() > 0 and await redirect_btn.first.is_visible():
            logger.info(f"[{task.task_id}] 检测到中国区 Apple ID 专属重定向，点击前往 iCloud.com.cn...")
            await redirect_btn.first.click(timeout=3000)
            redirected = True
        elif await js_click_by_text(page, "Go to iCloud.com.cn", "前往 iCloud.com.cn", "iCloud.com.cn"):
            logger.info(f"[{task.task_id}] 文本穿透点击了前往 iCloud.com.cn")
            redirected = True
    except Exception as exc:
        logger.debug("[%s] 中国区跳转按钮不可用 (%s)", task.task_id, type(exc).__name__)

    if redirected:
        task.req.host = "icloud.com.cn"
        if "icloud.com.cn" not in page.url:
            await page.goto("https://www.icloud.com.cn/", wait_until="domcontentloaded", timeout=25000)
        await _submit_credentials_to_page(task, page)
        return False

    # 2. 检查信任浏览器按钮（注意：必须排除“不信任”/“Don't Trust”，优先点击主要按钮）
    trust_selectors = [
        'button:text-is("信任")',
        'button:text-is("Trust")',
        'button.button-primary:has-text("信任")',
        'button.button-primary:has-text("Trust")',
        'button[id*="trust"]:not([id*="dont"]):not([id*="not"])',
        '[role="button"]:text-is("信任")',
        '[role="button"]:text-is("Trust")',
    ]
    clicked_trust = False
    for f in page.frames:
        for sel in trust_selectors:
            try:
                btn = f.locator(sel)
                if await btn.count() > 0 and await btn.first.is_visible():
                    txt = (await btn.first.inner_text()).strip()
                    if "不" not in txt and "don't" not in txt.lower():
                        await btn.first.click(timeout=2000)
                        logger.info(f"[{task.task_id}] 已精准点击【信任此浏览器】(选择器: {sel}, 文本: '{txt}')")
                        clicked_trust = True
                        break
            except Exception:
                pass
        if clicked_trust:
            break
        try:
            if await js_click_by_text(f, "信任", "Trust"):
                logger.info(f"[{task.task_id}] 文本穿透点击了【信任此浏览器】")
                clicked_trust = True
                break
        except Exception:
            pass

    if clicked_trust:
        logger.info(f"[{task.task_id}] 已点击【信任】，等待登录 iframe 完成并让主页面建立长效会话...")
        try:
            # 等待登录 iframe 销毁，代表身份验证已向主窗口交付
            await page.wait_for_selector('iframe[name="aid-auth-widget"], iframe#aid-auth-widget-iFrame', state="detached", timeout=15000)
            logger.info(f"[{task.task_id}] 登录 iframe 已顺利销毁，正在加载主页面长效凭据...")
        except Exception:
            pass
        # 轮询等待主页加载长效凭据 (优先等待 X-APPLE-WEB-ID 或完整长效 TOKEN)
        for _ in range(8):
            await asyncio.sleep(1)
            raw = await context.cookies()
            names = {c.get("name") for c in raw if c.get("name")}
            if "X-APPLE-WEB-ID" in names or ("X-APPLE-WEBAUTH-TOKEN" in names and "X-APPLE-WEBAUTH-HSA-TRUST" in names):
                logger.info(f"[{task.task_id}] 已成功捕获主页长效凭据 (含 X-APPLE-WEB-ID: {'X-APPLE-WEB-ID' in names})")
                break

    return await check_login_success_and_sync(task, context)

async def _ensure_keep_signed_in(auth_frame, task_id: str = "") -> bool:
    """只在存在且未勾选时设置保持登录，不把可选控件当作登录前提。"""
    try:
        checkbox = auth_frame.locator('input#remember-me, input.form-choice-checkbox').first
        if await checkbox.count() == 0:
            return False
        if not await checkbox.is_checked():
            await checkbox.check(force=True, timeout=2000)
        return True
    except Exception as exc:
        logger.warning("[%s] 设置保持登录状态失败 (%s)", task_id, type(exc).__name__)
    return False

async def _submit_credentials_to_page(task: TaskState, page) -> None:
    # 点击主页面的登录按钮
    sign_in_btn = page.locator('ui-button:has-text("登录"), button:has-text("登录"), button:has-text("Sign In"), .sign-in-button, [data-test="sign-in-button"]')
    try:
        await sign_in_btn.first.wait_for(state="visible", timeout=8000)
        await sign_in_btn.first.click()
        logger.info(f"[{task.task_id}] 点击了【登录】按钮")
    except Exception:
        if await js_click_by_text(page, "登录", "Sign In", "Sign in"):
            logger.info(f"[{task.task_id}] 文本穿透点击了【登录】按钮")
        else:
            logger.info(f"[{task.task_id}] 未发现独立登录按钮，可能直接嵌入登录框")

    # 等待官方认证 iframe (结合 selector 与 frames URL 多路探测)
    logger.info(f"[{task.task_id}] 等待登录 iframe 加载...")
    auth_frame = None
    for _ in range(25):
        for f in page.frames:
            if "aid-auth-widget" in f.name or "idmsa.apple.com" in f.url or "appleid.apple.com" in f.url:
                auth_frame = f
                break
        if auth_frame:
            break
        await asyncio.sleep(1)

    if not auth_frame:
        try:
            frame_elem = await page.wait_for_selector('iframe[name="aid-auth-widget"], iframe#aid-auth-widget-iFrame, iframe[src*="idmsa.apple.com"], iframe[src*="appleid.apple.com"]', timeout=3000)
            if frame_elem:
                auth_frame = await frame_elem.content_frame()
        except Exception:
            pass

    if not auth_frame:
        raise Exception("未找到 Apple ID 登录窗口 (iframe 捕获超时)")

    # 步骤 1: 输入账号
    logger.info(f"[{task.task_id}] 输入用户名...")
    acc_input = auth_frame.locator('input#account_name_text_field, input[type="email"]').first
    await acc_input.wait_for(state="visible", timeout=15000)
    await acc_input.fill(task.req.username)
    await asyncio.sleep(0.5)

    # 步骤 2: 点击【继续】以展开密码输入框与保持登录选项
    continue_btn = auth_frame.locator('button#sign-in').first
    await continue_btn.click()

    # 步骤 3: 等待密码输入框
    pwd_input = auth_frame.locator('input#password_text_field').first
    await pwd_input.wait_for(state="visible", timeout=15000)

    # 步骤 4: 输入密码
    logger.info(f"[{task.task_id}] 正在填入密码...")
    await pwd_input.fill(task.req.password)
    await asyncio.sleep(0.5)

    # 步骤 5: 可选的保持登录状态
    await _ensure_keep_signed_in(auth_frame, task.task_id)
    await asyncio.sleep(1.5)

    # 步骤 6: 提交登录
    submit_btn = auth_frame.locator('button#sign-in').first
    await submit_btn.click()
    logger.info(f"[{task.task_id}] 已正式提交密码，进入状态机判定...")

async def _execute_login(task: TaskState, context, page) -> None:
    host_str = (task.req.host or "icloud.com").strip().lower()
    is_china = "icloud.com.cn" in host_str or "china" in host_str
    start_url = "https://www.icloud.com.cn/" if is_china else "https://www.icloud.com/"

    logger.info(f"[{task.task_id}] 访问 {start_url} (目标宿主: {host_str})...")
    await page.goto(start_url, wait_until="domcontentloaded", timeout=25000)
    await _submit_credentials_to_page(task, page)

    # 轮询判定分支：密码错误 / 2FA / 免密成功 / 中国区自愈重定向 (最长等 40 秒)
    deadline = asyncio.get_event_loop().time() + 40
    has_otp = False
    otp_digits_locator = None

    while asyncio.get_event_loop().time() < deadline:
        await asyncio.sleep(0.5)

        # 0. 检查是否已免密登录成功或已触发跳转/信任
        previous_host = task.req.host
        if await handle_redirect_and_trust(page, context, task):
            return
        if previous_host != task.req.host:
            deadline = asyncio.get_event_loop().time() + 40
            continue

        # 0.1 检查是否触发中国区重定向阻断（若此前配置为国际区）
        if "icloud.com.cn" not in page.url:
            try:
                page_text = await page.content()
            except Exception:
                page_text = ""
            if "can't sign in to icloud.com" in page_text.lower() or "前往 icloud.com.cn" in page_text:
                logger.info(f"[{task.task_id}] 发现该账号属于云上贵州(中国区)，自动切换至 https://www.icloud.com.cn/ 执行原生登录...")
                task.req.host = "icloud.com.cn"
                await page.goto("https://www.icloud.com.cn/", wait_until="domcontentloaded", timeout=25000)
                await _submit_credentials_to_page(task, page)
                deadline = asyncio.get_event_loop().time() + 40
                continue

        # 1. 检查各 frame 中是否存在账号密码错误或拦截提示
        for f in page.frames:
            err_box = f.locator('.form-message, #errMsg, [role="alert"], .error, .signin-error')
            if await err_box.count() > 0:
                for eb in await err_box.all():
                    if await eb.is_visible():
                        err_text = (await eb.inner_text()).strip()
                        if any(k in err_text.lower() for k in [
                            "check the account", "incorrect", "not valid", "try again",
                            "错误", "不正确", "重试", "locked", "锁定", "failed"
                        ]):
                            raise Exception(f"Apple ID 账号或密码错误: {err_text}")

        # 2. 检查是否触发 2FA 验证码输入框
        for f in page.frames:
            digits = f.locator(OTP_INPUT_SELECTOR)
            if await digits.count() > 0 and await digits.first.is_visible():
                otp_digits_locator = digits
                has_otp = True
                break
            try:
                f_text = await f.content()
                if any(k in f_text for k in ["Two-Factor Authentication", "双重认证", "Enter the verification code", "输入验证码"]):
                    if await digits.count() > 0:
                        otp_digits_locator = digits
                        has_otp = True
                        break
            except Exception:
                pass

        if has_otp:
            logger.info(f"[{task.task_id}] 命中 2FA 验证状态")
            break

    if has_otp:
        task.status = "otp_required"
        logger.info(f"[{task.task_id}] 触发双重认证，挂起等待用户在前端输入 6 位验证码...")
        try:
            # 等待外部通过 POST /submit-otp 传入验证码，最长等 180 秒
            otp_code = await asyncio.wait_for(task.otp_queue.get(), timeout=180)
        except asyncio.TimeoutError:
            raise Exception("等待双重认证验证码超时 (180s)")

        task.status = "verifying"
        logger.info(f"[{task.task_id}] 收到验证码，填入表单...")

        # 填入前先检查是否已经由设备端放行（例如在受信任设备上点击了“允许”直接重定向）
        if await handle_redirect_and_trust(page, context, task):
            return

        # 填入验证码
        try:
            # 优先方案：聚焦第一个格子，通过键盘事件顺序键入，完美顺应苹果官方自带的焦点自推进逻辑
            first_box = otp_digits_locator.first
            try:
                await first_box.click(timeout=2000)
            except Exception:
                await first_box.focus()

            await page.keyboard.type(otp_code, delay=100)
            logger.info(f"[{task.task_id}] 已完成 6 位验证码顺序键入")
        except Exception as e:
            logger.warning("[%s] 键盘流输入验证码出现告警，启用保底逐格填入 (%s)", task.task_id, type(e).__name__)
            try:
                box_count = await otp_digits_locator.count()
                if box_count == 1:
                    await otp_digits_locator.first.fill(otp_code)
                elif box_count >= len(otp_code):
                    for idx, digit in enumerate(otp_code):
                        await otp_digits_locator.nth(idx).fill(digit)
                else:
                    raise RuntimeError("验证码输入框数量不足")
            except Exception as fallback_exc:
                if await handle_redirect_and_trust(page, context, task):
                    return
                raise RuntimeError("双重认证验证码输入失败") from fallback_exc

            logger.info(f"[{task.task_id}] 验证码输入完成，等待后续验证与信任提示...")
            try:
                await otp_digits_locator.last.press("Enter", timeout=1000)
            except Exception:
                pass
        await asyncio.sleep(1)

    # 4. 检查并处理“信任此浏览器”与最终就绪（最长等 45 秒）
    post_deadline = asyncio.get_event_loop().time() + 45
    while asyncio.get_event_loop().time() < post_deadline:
        # 检查验证码是否输入错误
        for f in page.frames:
            err_box = f.locator('.form-message, #errMsg, [role="alert"], .error')
            if await err_box.count() > 0:
                for eb in await err_box.all():
                    if await eb.is_visible():
                        err_text = (await eb.inner_text()).strip()
                        if any(k in err_text.lower() for k in ["incorrect", "code", "错误", "验证码", "try again"]):
                            raise Exception("双重认证验证码错误")

        if await handle_redirect_and_trust(page, context, task):
            return

        await asyncio.sleep(1)

    # 最终保底：尝试读取全域 Cookies
    if not await handle_redirect_and_trust(page, context, task):
        raise Exception("Apple ID 认证未能完成，未提取到有效登录态 Cookie")


def camoufox_proxy_settings(proxy_url: str) -> Dict[str, str]:
    try:
        parsed = urlsplit(proxy_url)
        hostname = parsed.hostname
        port = parsed.port
    except ValueError as exc:
        raise ValueError("代理地址格式无效") from exc
    if not parsed.scheme or not hostname or parsed.path not in ("", "/") or parsed.query or parsed.fragment:
        raise ValueError("代理地址格式无效")
    if parsed.scheme in ("socks4", "socks5", "socks5h") and parsed.username is not None:
        raise ValueError("代理协议不支持认证")
    host = f"[{hostname}]" if ":" in hostname else hostname
    server = urlunsplit((parsed.scheme, f"{host}:{port}" if port else host, "", "", ""))
    settings = {"server": server}
    if parsed.username is not None:
        settings["username"] = unquote(parsed.username)
    if parsed.password is not None:
        settings["password"] = unquote(parsed.password)
    return settings


async def run_camoufox_worker(task: TaskState):
    if AsyncCamoufox is None:
        task.status = "failed"
        task.error_message = "Camoufox 未安装，请先执行 pip install 'camoufox[geoip]'"
        return

    try:
        proxy_cfg = camoufox_proxy_settings(task.req.proxy) if task.req.proxy else None
        logger.info("[%s] 启动 Camoufox 实例 (代理已配置: %s)", task.task_id, bool(proxy_cfg))
        headless_env = os.environ.get("CAMOUFOX_HEADLESS")
        if headless_env is not None:
            is_headless = headless_env.lower() in ("true", "1")
        else:
            # 未显式配置时：在无 DISPLAY 的 Linux 服务器环境默认开启无头，Windows 桌面环境默认有头
            is_headless = True if (sys.platform.startswith("linux") and not os.environ.get("DISPLAY")) else False

        logger.info(f"[{task.task_id}] 启动 Camoufox 实例 (headless={is_headless}, platform={sys.platform})...")
        async with AsyncCamoufox(
            headless=is_headless,
            os="windows",
            proxy=proxy_cfg,
            humanize=True,
            screen=Screen(max_width=1920, max_height=1080),
            enable_cache=False,  # 避免多账号会话污染
        ) as browser:
            context = await browser.new_context(
                viewport={"width": 1024, "height": 720}, # 降低合成重绘开销
                reduced_motion="reduce",                 # 禁用 CSS 位移动画，加速表单稳定
                service_workers="block",                 # 阻断 service worker 缓存
            )
            page = await context.new_page()
            await _execute_login(task, context, page)

    except Exception as e:
        task.status = "failed"
        task.error_message = _safe_login_error(e)
        logger.error("[%s] 登录失败 (%s)", task.task_id, type(e).__name__)

async def run_task(task: TaskState) -> None:
    try:
        await asyncio.wait_for(run_camoufox_worker(task), timeout=MAX_TASK_SECONDS)
    except asyncio.TimeoutError:
        task.status = "failed"
        task.error_message = "登录任务超时"
    finally:
        task.req.password = ""
        task.req.proxy = None
        task.worker = None
        active_tasks.discard(task.task_id)
        asyncio.get_running_loop().call_later(TASK_RESULT_SECONDS, tasks.pop, task.task_id, None)


async def _probe_camoufox_runtime() -> bool:
    """启动最小浏览器实例，确认内核不仅存在，而且当前仍可运行。"""
    if AsyncCamoufox is None or not camoufox_ready:
        return False
    try:
        async with AsyncCamoufox(headless=True, os="windows", enable_cache=False) as browser:
            context = await browser.new_context(viewport={"width": 640, "height": 480})
            await context.close()
        return True
    except Exception as exc:
        logger.warning("[-] Camoufox 健康探针启动失败 (%s)", type(exc).__name__)
        return False


async def refresh_camoufox_runtime_ready() -> None:
    global camoufox_ready, health_probe_result
    if not camoufox_ready and AsyncCamoufox is not None:
        camoufox_ready = await asyncio.to_thread(ensure_camoufox_browser)
    if not camoufox_ready:
        health_probe_result = False
        return
    try:
        health_probe_result = await asyncio.wait_for(
            _probe_camoufox_runtime(), timeout=HEALTH_PROBE_TIMEOUT_SECONDS
        )
    except asyncio.TimeoutError:
        logger.warning("[-] Camoufox 健康探针启动超时")
        health_probe_result = False


async def refresh_camoufox_runtime_periodically() -> None:
    while True:
        delay = HEALTH_PROBE_INTERVAL_SECONDS if health_probe_result else HEALTH_PROBE_RETRY_SECONDS
        await asyncio.sleep(delay)
        while active_tasks:
            await asyncio.sleep(HEALTH_PROBE_RETRY_SECONDS)
        await refresh_camoufox_runtime_ready()

@app.post("/login", dependencies=[Depends(require_token)])
async def start_login(req: LoginRequest):
    if not health_probe_result:
        raise HTTPException(status_code=503, detail="Camoufox 浏览器内核未就绪")
    if len(active_tasks) >= MAX_ACTIVE_TASKS:
        raise HTTPException(status_code=429, detail="登录任务已达并发上限，请稍后重试")

    task_id = "task_" + uuid.uuid4().hex[:12]
    task = TaskState(task_id, req)
    tasks[task_id] = task
    active_tasks.add(task_id)
    task.worker = asyncio.create_task(run_task(task))
    return {
        "success": True,
        "task_id": task_id,
        "status": "initializing"
    }

@app.post("/submit-otp", dependencies=[Depends(require_token)])
async def submit_otp(req: SubmitOTPRequest):
    task = tasks.get(req.task_id)
    if not task:
        raise HTTPException(status_code=404, detail="任务不存在或已过期")

    if task.status == "success":
        return {"success": True, "task_id": req.task_id, "status": "success"}
    if task.status == "failed":
        raise HTTPException(status_code=410, detail="登录任务已结束，请重新登录")
    if task.status == "verifying":
        raise HTTPException(status_code=409, detail="验证码正在验证，请等待结果")
    if task.status != "otp_required":
        raise HTTPException(status_code=400, detail=f"当前任务状态为 {task.status}，非等待验证码状态")

    try:
        task.otp_queue.put_nowait(req.otp_code)
    except asyncio.QueueFull:
        raise HTTPException(status_code=409, detail="验证码已提交")
    task.status = "verifying"

    return {
        "success": True,
        "task_id": req.task_id,
        "status": "verifying"
    }

@app.get("/tasks/{task_id}", dependencies=[Depends(require_token)])
async def get_task_status(task_id: str):
    task = tasks.get(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="任务不存在")

    res = {
        "task_id": task.task_id,
        "status": task.status,
        "host": task.req.host,
        "error_message": task.error_message,
        "has_cookies": task.cookies is not None,
        "account_id": task.req.account_id
    }
    if task.status == "success" and task.cookies:
        res["cookie_count"] = len(task.cookies)
        res["cookies"] = task.cookies
    return res

@app.delete("/tasks/{task_id}", dependencies=[Depends(require_token)])
async def cancel_task(task_id: str):
    task = tasks.pop(task_id, None)
    if task and task.status not in ("success", "failed") and task.worker and not task.worker.done():
        task.worker.cancel()
    return {"success": task is not None}

@app.get("/health", dependencies=[Depends(require_token)])
async def health():
    return {"status": "ok", "camoufox_ready": health_probe_result}

if __name__ == "__main__":
    import uvicorn
    port = int(os.environ.get("CAMOUFOX_AGENT_PORT", os.environ.get("PORT", "8089")))
    bind_host = os.environ.get("CAMOUFOX_AGENT_HOST", "127.0.0.1")
    uvicorn.run("main:app", host=bind_host, port=port, reload=False)
