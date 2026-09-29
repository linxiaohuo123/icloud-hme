import asyncio
import os
import time
import unittest
from unittest.mock import patch

from fastapi.testclient import TestClient

import main


class AgentAPITest(unittest.TestCase):
    def setUp(self):
        self.env = patch.dict(os.environ, {"ICLOUD_HME_CAMOUFOX_TOKEN": "test-token"})
        self.env.start()
        self.ready = patch.object(main, "camoufox_ready", True)
        self.ready.start()
        self.addCleanup(self.env.stop)
        self.addCleanup(self.ready.stop)
        main.tasks.clear()
        main.active_tasks.clear()

    def test_authentication_and_task_result_cleanup(self):
        async def completed(task):
            task.cookies = {"X-APPLE-WEB-ID": "test-cookie"}
            task.status = "success"

        with patch.object(main, "run_camoufox_worker", completed), patch.object(main, "TASK_RESULT_SECONDS", 0.05):
            with TestClient(main.app) as client:
                self.assertEqual(client.get("/health").status_code, 401)
                headers = {"X-Camoufox-Token": "test-token"}
                self.assertEqual(client.get("/health", headers=headers).status_code, 200)
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
                time.sleep(0.1)
                self.assertEqual(client.get(f"/tasks/{task_id}", headers=headers).status_code, 404)

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

        class AuthenticatedContext:
            async def cookies(self):
                return [
                    {"name": "X-APPLE-WEBAUTH-TOKEN", "value": "token"},
                    {"name": "X-APPLE-WEBAUTH-USER", "value": "user"},
                ]

        self.assertTrue(asyncio.run(main.check_login_success_and_sync(task, AuthenticatedContext())))
        self.assertEqual(task.status, "success")


if __name__ == "__main__":
    unittest.main()
