import asyncio
import os
import time
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

from fastapi.testclient import TestClient

import main


class AgentAPITest(unittest.TestCase):
    def setUp(self):
        self.env = patch.dict(os.environ, {"ICLOUD_HME_CAMOUFOX_TOKEN": "test-token"})
        self.env.start()
        self.ready = patch.object(main, "camoufox_ready", True)
        self.ready.start()
        self.probe = patch.object(main, "_probe_camoufox_runtime", new_callable=AsyncMock, return_value=True)
        self.probe_mock = self.probe.start()
        self.addCleanup(self.env.stop)
        self.addCleanup(self.ready.stop)
        self.addCleanup(self.probe.stop)
        main.tasks.clear()
        main.active_tasks.clear()
        main.health_probe_result = False

    def test_authentication_and_task_result_cleanup(self):
        async def completed(task):
            task.cookies = {"X-APPLE-WEB-ID": "test-cookie"}
            task.status = "success"

        with patch.object(main, "run_camoufox_worker", completed), patch.object(main, "TASK_RESULT_SECONDS", 0.05):
            with TestClient(main.app) as client:
                self.assertEqual(client.get("/health").status_code, 401)
                headers = {"X-Camoufox-Token": "test-token"}
                self.assertEqual(client.get("/health", headers=headers).status_code, 200)
                self.assertEqual(client.get("/health", headers=headers).json()["camoufox_ready"], True)
                response = client.post("/login", headers=headers, json={
                    "account_id": "account-1", "username": "test@example.com", "password": "secret"
                })
                self.assertEqual(response.status_code, 200)
                task_id = response.json()["task_id"]
                for _ in range(20):
                    if main.tasks[task_id].status == "success" and main.tasks[task_id].req.password == "":
                        break
                    time.sleep(0.005)
                self.assertEqual(main.tasks[task_id].req.password, "")
                self.assertEqual(client.get(f"/tasks/{task_id}").status_code, 401)
                self.assertEqual(client.get(f"/tasks/{task_id}", headers=headers).json()["cookies"], {
                    "X-APPLE-WEB-ID": "test-cookie"
                })
                self.assertEqual(client.get(f"/tasks/{task_id}", headers=headers).json()["host"], "icloud.com")
                time.sleep(0.1)
                self.assertEqual(client.get(f"/tasks/{task_id}", headers=headers).status_code, 404)
                self.probe_mock.assert_awaited_once()

    def test_concurrency_limit_and_cancellation(self):
        async def stalled(task):
            await asyncio.Event().wait()

        with patch.object(main, "run_camoufox_worker", stalled):
            with TestClient(main.app) as client:
                headers = {"X-Camoufox-Token": "test-token"}
                payload = {"account_id": "account-1", "username": "test@example.com", "password": "secret"}
                first = client.post("/login", headers=headers, json=payload)
                second = client.post("/login", headers=headers, json=payload)
                self.assertEqual(first.status_code, 200)
                self.assertEqual(second.status_code, 200)
                self.assertEqual(client.post("/login", headers=headers, json=payload).status_code, 429)
                for response in (first, second):
                    self.assertEqual(client.delete(f"/tasks/{response.json()['task_id']}", headers=headers).status_code, 200)
                for _ in range(20):
                    if not main.active_tasks:
                        break
                    time.sleep(0.005)
                self.assertFalse(main.active_tasks)

    def test_failed_runtime_probe_rejects_login(self):
        self.probe_mock.return_value = False
        with TestClient(main.app) as client:
            headers = {"X-Camoufox-Token": "test-token"}
            self.assertFalse(client.get("/health", headers=headers).json()["camoufox_ready"])
            response = client.post("/login", headers=headers, json={
                "account_id": "account-1", "username": "test@example.com", "password": "secret"
            })
            self.assertEqual(response.status_code, 503)
            self.probe_mock.assert_awaited_once()

    def test_runtime_probe_recovers_after_initial_install_failure(self):
        main.camoufox_ready = False
        with patch.object(main, "ensure_camoufox_browser", return_value=True) as install:
            asyncio.run(main.refresh_camoufox_runtime_ready())
        install.assert_called_once()
        self.assertTrue(main.camoufox_ready)
        self.assertTrue(main.health_probe_result)
        self.probe_mock.assert_awaited_once()

    def test_proxy_credentials_are_separate_from_server(self):
        self.assertEqual(main.camoufox_proxy_settings("http://user:p%40ss@proxy.example:8080"), {
            "server": "http://proxy.example:8080", "username": "user", "password": "p@ss",
        })
        self.assertEqual(main.camoufox_proxy_settings("http://[::1]:8080"), {
            "server": "http://[::1]:8080",
        })
        with self.assertRaises(ValueError):
            main.camoufox_proxy_settings("socks5://user:secret@proxy.example:1080")

    def test_repeated_otp_does_not_requeue(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))
        task.status = "otp_required"
        main.tasks[task.task_id] = task
        with TestClient(main.app) as client:
            headers = {"X-Camoufox-Token": "test-token"}
            payload = {"task_id": task.task_id, "otp_code": "123456"}
            self.assertEqual(client.post("/submit-otp", headers=headers, json=payload).status_code, 200)
            self.assertEqual(client.post("/submit-otp", headers=headers, json=payload).status_code, 409)
            self.assertEqual(client.post("/submit-otp", headers=headers, json={
                "task_id": task.task_id, "otp_code": "654321"
            }).status_code, 409)
            self.assertEqual(task.otp_queue.qsize(), 1)

    def test_completed_otp_task_returns_success_and_failed_task_expires(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))
        main.tasks[task.task_id] = task
        with TestClient(main.app) as client:
            headers = {"X-Camoufox-Token": "test-token"}
            payload = {"task_id": task.task_id, "otp_code": "123456"}
            task.status = "success"
            self.assertEqual(client.post("/submit-otp", headers=headers, json=payload).json()["status"], "success")
            task.status = "failed"
            self.assertEqual(client.post("/submit-otp", headers=headers, json=payload).status_code, 410)

    def test_otp_requires_six_ascii_digits(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))
        task.status = "otp_required"
        main.tasks[task.task_id] = task
        with TestClient(main.app) as client:
            headers = {"X-Camoufox-Token": "test-token"}
            for value in ("12345", "1234567", "12ab56", "１２３４５６"):
                response = client.post("/submit-otp", headers=headers, json={
                    "task_id": task.task_id, "otp_code": value
                })
                self.assertEqual(response.status_code, 422, value)

    def test_web_id_alone_does_not_complete_login(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))

        class CookieContext:
            async def cookies(self):
                return [{"name": "X-APPLE-WEB-ID", "value": "web-id"}]

        self.assertFalse(asyncio.run(main.check_login_success_and_sync(task, CookieContext())))
        self.assertEqual(task.status, "initializing")

        class PartialContext:
            async def cookies(self):
                return [
                    {"name": "X-APPLE-WEBAUTH-TOKEN", "value": "token"},
                    {"name": "X-APPLE-WEBAUTH-USER", "value": "user"},
                ]

        self.assertFalse(asyncio.run(main.check_login_success_and_sync(task, PartialContext())))
        self.assertEqual(task.status, "initializing")

        class AuthenticatedContext:
            async def cookies(self):
                return [
                    {"name": "X-APPLE-WEBAUTH-TOKEN", "value": "token"},
                    {"name": "X-APPLE-WEBAUTH-HSA-TRUST", "value": "trust"},
                ]

        self.assertTrue(asyncio.run(main.check_login_success_and_sync(task, AuthenticatedContext())))
        self.assertEqual(task.status, "success")

    def test_china_redirect_submits_credentials_after_navigation(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))
        page = MagicMock()
        page.frames = []
        page.url = "https://www.icloud.com/"
        redirect = MagicMock()
        redirect.count = AsyncMock(return_value=1)
        redirect.first.is_visible = AsyncMock(return_value=True)

        async def click(**_kwargs):
            page.url = "https://www.icloud.com.cn/"

        redirect.first.click = AsyncMock(side_effect=click)
        page.locator.return_value = redirect
        context = MagicMock()
        context.cookies = AsyncMock(return_value=[])

        with patch.object(main, "_submit_credentials_to_page", new_callable=AsyncMock) as submit:
            self.assertFalse(asyncio.run(main.handle_redirect_and_trust(page, context, task)))
            submit.assert_awaited_once_with(task, page)
        self.assertEqual(task.req.host, "icloud.com.cn")

    def test_keep_signed_in_only_checks_unchecked_control(self):
        frame = MagicMock()
        checkbox = frame.locator.return_value.first
        checkbox.count = AsyncMock(return_value=1)
        checkbox.is_checked = AsyncMock(return_value=True)
        checkbox.check = AsyncMock()

        self.assertTrue(asyncio.run(main._ensure_keep_signed_in(frame, "test-task")))
        checkbox.check.assert_not_awaited()

        checkbox.is_checked.return_value = False
        self.assertTrue(asyncio.run(main._ensure_keep_signed_in(frame, "test-task")))
        checkbox.check.assert_awaited_once()

        checkbox.count.return_value = 0
        self.assertFalse(asyncio.run(main._ensure_keep_signed_in(frame, "test-task")))

    def test_visible_otp_blocks_partial_cookie_success(self):
        task = main.TaskState("test-task", main.LoginRequest(
            account_id="account-1", username="test@example.com", password="secret"
        ))
        frame = MagicMock()
        digits = frame.locator.return_value
        digits.count = AsyncMock(return_value=1)
        digits.first.is_visible = AsyncMock(return_value=True)
        page = MagicMock()
        page.frames = [frame]
        context = MagicMock()
        context.cookies = AsyncMock(return_value=[
            {"name": "X-APPLE-WEBAUTH-TOKEN", "value": "token"},
            {"name": "X-APPLE-WEBAUTH-HSA-TRUST", "value": "trust"},
        ])

        self.assertFalse(asyncio.run(main.handle_redirect_and_trust(page, context, task)))
        context.cookies.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
