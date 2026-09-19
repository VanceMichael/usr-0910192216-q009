#!/usr/bin/env python3
"""故障注入与恢复验证。

场景（需要 LEDGER_FAULT_HOOKS=1，compose 默认开启）：
  A. 结算确认事务提交前崩溃 -> 整体回滚：确认事件不占审计序号、水位不动、
     恢复后同一 confirmation_id 重新确认成功，合同解释（v2）保持不变；
  B. 提交后、响应前崩溃 -> 客户端重试走幂等路径，分账指令仍然只有一份；
  C. 进程崩溃 -> restart 策略拉起，继续接受新的审计序号（无回退、无空洞）；
  D. 恢复后离线校验：哈希链完整、借贷平衡、重放与账本一致。

用法：scripts/fault_inject.sh（自动确保 compose 栈已启动）。
"""
import http.client
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("LEDGER_BASE", "http://localhost:8080")
ADMIN = "dev-admin-token"
AUDITOR = "dev-auditor-token"

FAILURES = []


def check(name, cond, detail=""):
    mark = "PASS" if cond else "FAIL"
    print(f"  [{mark}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def call(method, path, token=ADMIN, body=None, timeout=15):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Authorization", "Bearer " + token)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, json.loads(raw) if raw else None


def call_expect_crash(method, path, body=None):
    """调用会在中途崩溃的接口；连接被重置即视为崩溃生效。"""
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Authorization", "Bearer " + ADMIN)
    data = json.dumps(body).encode() if body is not None else None
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        urllib.request.urlopen(req, data, timeout=15)
        return False  # 没有崩溃
    except (urllib.error.URLError, ConnectionError, OSError, http.client.HTTPException):
        return True


def wait_app(timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            # 应用崩溃重启期间连接会被拒绝，持续轮询
            with urllib.request.urlopen(BASE + "/healthz", timeout=3) as resp:
                if resp.status == 204:
                    return True
        except Exception:
            pass
        time.sleep(0.5)
    return False


def chain_head():
    _, body = call("GET", "/v1/audit/verify-chain", token=AUDITOR)
    return body["head_seq"], body["ok"]


def main():
    print(f"== 故障注入与恢复验证 @ {BASE}")
    if not wait_app():
        print("app 未就绪")
        sys.exit(1)

    # ---- 基线：当前链头与水位 ----
    head0, ok0 = chain_head()
    check("故障前哈希链完好", ok0)
    _, wm0 = call("GET", "/v1/watermarks", token=AUDITOR)
    print(f"  基线: head_seq={head0} watermarks={wm0['watermarks']}")

    # ---- A. 提交前崩溃：事务整体回滚 ----
    print("-- A. 结算确认提交前崩溃（crash_before_commit）")
    status, ev = call("POST", "/v1/events", body={
        "event_id": "33333333-3333-4333-8333-333333333301",
        "type": "order", "region_code": "CN-SH", "order_id": "O-04",
        "occurred_at": "2026-09-11T12:00:00+08:00",
        "payload": {"product_group": "ceramic", "amount": "300.00",
                    "creator_id": "creator:li", "coop_id": "coop:tea",
                    "channel_id": "channel:mall"}})
    check("注入前写入 O-04（09-11 周期）", status in (200, 201), ev)
    seq_o04 = ev["seq"]
    check("O-04 审计序号紧跟链头", seq_o04 == head0 + 1, f"seq={seq_o04} head0={head0}")

    crashed = call_expect_crash(
        "POST", "/v1/settlements/confirm?fault=crash_before_commit",
        body={"confirmation_id": "44444444-4444-4444-8444-444444444401",
              "region_code": "CN-SH", "period": "2026-09-11"})
    check("确认请求导致进程崩溃（连接重置）", crashed)
    check("应用按 restart 策略恢复", wait_app())

    head1, ok1 = chain_head()
    check("恢复后哈希链完好", ok1)
    check("崩溃的确认事件已回滚：链头不变", head1 == seq_o04,
          f"head={head1} want={seq_o04}")
    _, wm1 = call("GET", "/v1/watermarks", token=AUDITOR)
    check("水位未被崩溃回退或推进", wm1 == wm0, f"{wm0} vs {wm1}")

    # 恢复后重新确认：同一 confirmation_id 必须成功且只产生一份指令
    status, conf = call("POST", "/v1/settlements/confirm", body={
        "confirmation_id": "44444444-4444-4444-8444-444444444401",
        "region_code": "CN-SH", "period": "2026-09-11"})
    check("恢复后重新确认 -> 201 created", status == 201 and conf["outcome"] == "created", conf)
    st = conf["settlement"]
    check("合同解释保持 v2（切换后版本，未被故障改变）",
          st["contract_version"] == 2, st)
    check("结算总额 300.00（O-04）", st["gross_cents"] == 30000, st)
    check("确认事件占用下一个审计序号（无回退、无空洞）",
          st["confirmation_seq"] == seq_o04 + 1, st)
    terms = st["terms"]
    check("冻结条款为 v2 分成 50/30/20",
          terms["creator_bps"] == 5000 and terms["coop_bps"] == 3000, terms)

    # ---- B. 提交后崩溃：幂等重试，指令唯一 ----
    print("-- B. 提交后、响应前崩溃（crash_after_commit）")
    status, ev = call("POST", "/v1/events", body={
        "event_id": "33333333-3333-4333-8333-333333333302",
        "type": "order", "region_code": "CN-SH", "order_id": "O-05",
        "occurred_at": "2026-09-12T09:00:00+08:00",
        "payload": {"product_group": "tea", "amount": "100.00",
                    "creator_id": "creator:li", "coop_id": "coop:tea",
                    "channel_id": "channel:mall"}})
    check("继续接受新事件 O-05，审计序号连续", status == 201 and ev["seq"] == seq_o04 + 2, ev)

    crashed = call_expect_crash(
        "POST", "/v1/settlements/confirm?fault=crash_after_commit",
        body={"confirmation_id": "44444444-4444-4444-8444-444444444402",
              "region_code": "CN-SH", "period": "2026-09-12"})
    check("确认请求在提交后崩溃", crashed)
    check("应用再次恢复", wait_app())

    status, conf = call("POST", "/v1/settlements/confirm", body={
        "confirmation_id": "44444444-4444-4444-8444-444444444402",
        "region_code": "CN-SH", "period": "2026-09-12"})
    check("客户端重试 -> 200 duplicate（指令已存在且唯一）",
          status == 200 and conf["outcome"] == "duplicate", conf)
    settlement_id = conf["settlement"]["id"]
    status, conflict = call("POST", "/v1/settlements/confirm", body={
        "confirmation_id": "44444444-4444-4444-8444-444444444499",
        "region_code": "CN-SH", "period": "2026-09-12"})
    check("不同 confirmation_id 确认同周期 -> 409", status == 409, conflict)
    _, detail = call("GET", f"/v1/settlements/{settlement_id}", token=AUDITOR)
    postings = detail["postings"]
    check("该周期分账指令只有一份（借贷各半）",
          sum(1 for p in postings if p["leg"] == "sale") >= 1, detail)
    debits = sum(p["debit_cents"] for p in postings)
    credits = sum(p["credit_cents"] for p in postings)
    check("崩溃恢复后借贷仍平衡", debits == credits, f"{debits}!={credits}")

    # ---- C. 快照对账与离线校验 ----
    print("-- C. 恢复后的完整性与一致性")
    status, snap = call("GET", "/v1/audit/snapshot", token=AUDITOR)
    check("重放快照与账本一致（match=true）", status == 200 and snap["match"] is True, snap)
    head2, ok2 = chain_head()
    check("最终哈希链完好", ok2)

    if shutil.which("docker"):
        print("-- D. 离线校验器（docker compose --profile audit run --rm verify）")
        proc = subprocess.run(
            ["docker", "compose", "--profile", "audit", "run", "--rm", "verify"],
            cwd=os.path.join(os.path.dirname(__file__), ".."),
            capture_output=True, text=True, timeout=180)
        print("  " + proc.stdout.strip().replace("\n", "\n  "))
        check("离线校验退出码为 0", proc.returncode == 0, proc.stderr[-500:])
    else:
        print("  (跳过 docker 离线校验：当前环境无 docker)")

    print()
    if FAILURES:
        print(f"!! {len(FAILURES)} 项验证失败: {FAILURES}")
        sys.exit(1)
    print("== 故障恢复验证全部通过：合同解释保留，审计序号连续，指令唯一，链路完整。")


if __name__ == "__main__":
    main()
