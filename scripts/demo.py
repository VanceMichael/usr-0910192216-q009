#!/usr/bin/env python3
"""端到端演示：分账争议场景的全部关键证据。

前提：compose 栈已启动（scripts/up.sh）。在全新栈上运行本脚本
（如需重置：scripts/reset.sh）；故障注入脚本请在本演示之后运行。
固定 event_id 使事件写入幂等，重复执行不会重复记账。

演示并断言：
  1. 跨午夜订单/退货按地域时区归入正确周期；
  2. 合同切换日（2026-09-10）新销售用 v2，退货冲正按原订单周期用 v1；
  3. 重复确认只产生一次有效分账指令；不同 confirmation_id 确认同周期 -> 409；
  4. 迟到旧事件 late=true，可审计但不计入结算、不回退水位；
  5. 参与方只能读自己的金额；越权访问被拒；
  6. 审计快照与账本对账一致，两次重算差异正确；
  7. 哈希链校验通过（原始结算未被改写）。
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("LEDGER_BASE", "http://localhost:8080")
FIXTURES = os.path.join(os.path.dirname(__file__), "..", "fixtures", "分账事件.json")

TOKENS = {
    "admin": "dev-admin-token",
    "auditor": "dev-auditor-token",
    "creator": "dev-creator-token",
    "coop": "dev-coop-token",
    "channel": "dev-channel-token",
    "platform": "dev-platform-token",
}

FAILURES = []


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    if token:
        req.add_header("Authorization", "Bearer " + TOKENS[token])
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, json.loads(raw) if raw else None


def check(name, cond, detail=""):
    mark = "PASS" if cond else "FAIL"
    print(f"  [{mark}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def cents(v):
    return f"{v / 100:.2f}"


def main():
    print(f"== 分账账本端到端演示 @ {BASE}")

    # ---- 1. 按夹具顺序执行事件流 ----
    print("-- 1. 事件流（订单/退货/确认/迟到事件）")
    with open(FIXTURES, encoding="utf-8") as f:
        steps = json.load(f)["steps"]

    ingested = {}
    confirms = {}
    for step in steps:
        name = step["name"]
        print(f"  >> {name}")
        if "ingest" in step:
            status, body = call("POST", "/v1/events", "admin", step["ingest"])
            ev = step["ingest"]
            check(f"ingest {ev['order_id']} -> 201/200", status in (200, 201), body)
            ingested[ev["order_id"]] = body
        else:
            status, body = call("POST", "/v1/settlements/confirm", "admin", step["confirm"])
            confirms.setdefault(step["confirm"]["period"], []).append((status, body))

    # ---- 2. 周期归属（跨午夜，地域时区） ----
    print("-- 2. 跨午夜归属（Asia/Shanghai）")
    check("O-01 (23:50) 归 2026-09-09", ingested["O-01"]["period"] == "2026-09-09")
    check("O-02 (00:10) 归 2026-09-10", ingested["O-02"]["period"] == "2026-09-10")
    check("R-01 (00:05 退货) 归 2026-09-10", ingested["R-01"]["period"] == "2026-09-10")
    check("O-00 迟到事件 late=true", ingested["O-00"]["late"] is True)
    check("O-01 非迟到", ingested["O-01"]["late"] is False)

    # ---- 3. 幂等确认与冲突 ----
    print("-- 3. 重复确认只产生一次有效分账指令")
    c1_first, c1_dup = confirms["2026-09-09"][0], confirms["2026-09-09"][1]
    check("首次确认 09-09 -> created（重复执行时为 duplicate）",
          c1_first[0] in (200, 201) and c1_first[1]["outcome"] in ("created", "duplicate"), c1_first)
    check("重复确认 09-09 -> 200 duplicate",
          c1_dup[0] == 200 and c1_dup[1]["outcome"] == "duplicate", c1_dup)
    check("两次返回同一结算 id",
          c1_first[1]["settlement"]["id"] == c1_dup[1]["settlement"]["id"])
    c2_conflict = confirms["2026-09-10"][1]
    check("不同 confirmation_id 确认 09-10 -> 409 conflict",
          c2_conflict[0] == 409 and c2_conflict[1]["outcome"] == "conflict", c2_conflict)

    # ---- 4. 合同切换日与独立冲正 ----
    print("-- 4. 合同切换日版本与独立冲正腿")
    s1 = c1_first[1]["settlement"]
    check("09-09 结算用合同 v1", s1["contract_version"] == 1, s1)
    c2_first = confirms["2026-09-10"][0]
    check("确认 09-10 -> created（重复执行时为 duplicate）",
          c2_first[0] in (200, 201) and c2_first[1]["outcome"] in ("created", "duplicate"), c2_first)
    s2 = c2_first[1]["settlement"]
    check("09-10 结算用合同 v2", s2["contract_version"] == 2, s2)
    postings = c2_first[1]["postings"]
    legs = {p["leg"] for p in postings}
    check("销售腿与冲正腿并存（独立冲正）", legs == {"sale", "reversal"}, postings)
    rev_creator = next(p for p in postings if p["leg"] == "reversal"
                       and p["participant_id"] == "creator:li")
    check("冲正按原订单 v1 条款冲回创作者 351.00",
          rev_creator["debit_cents"] == 35100, rev_creator)
    check("冲正依据注明原合同 v1 / 原周期 2026-09-09",
          rev_creator["basis"]["original_contract_version"] == 1
          and rev_creator["basis"]["original_period"] == "2026-09-09", rev_creator)
    sale_creator = next(p for p in postings if p["leg"] == "sale"
                        and p["participant_id"] == "creator:li")
    check("切换日销售按 v2 条款（585.00 + 242.50 = 827.50）",
          sale_creator["credit_cents"] == 82750, sale_creator)
    debits = sum(p["debit_cents"] for p in postings)
    credits = sum(p["credit_cents"] for p in postings)
    check("09-10 结算借贷平衡", debits == credits, f"debits={debits} credits={credits}")

    # ---- 5. 参与方访问控制 ----
    print("-- 5. 参与方只能读取自己的金额与公开依据")
    status, st_creator = call("GET", "/v1/statements", "creator")
    check("创作者读自己的对账单 -> 200", status == 200, st_creator)
    total = sum(e["credit_cents"] - e["debit_cents"] for e in st_creator["entries"])
    check("创作者净额 827.50（351.00 + 827.50 - 351.00）", total == 82750, st_creator)
    check("对账单只含本人分录",
          all(e["account"].startswith("creator:li") for e in st_creator["entries"]))
    check("每行都带公开依据（合同版本）",
          all("contract_version" in e["basis"] for e in st_creator["entries"]))
    status, _ = call("GET", "/v1/audit/snapshot", "creator")
    check("创作者访问审计接口 -> 403", status == 403)
    status, _ = call("GET", "/v1/settlements/1", "creator")
    check("创作者访问结算明细 -> 403", status == 403)
    status, _ = call("GET", "/v1/statements")
    check("无 token -> 401", status == 401)
    status, st_coop = call("GET", "/v1/statements", "coop")
    coop_total = sum(e["credit_cents"] - e["debit_cents"] for e in st_coop["entries"])
    check("合作社净额 496.50（273.00 + 351.00 + 145.50 - 273.00）",
          coop_total == 49650, st_coop)

    # ---- 6. 迟到事件可审计但不回退水位 ----
    print("-- 6. 迟到旧事件与水位的单调性")
    status, evs = call("GET", "/v1/audit/events", "auditor")
    late = [e for e in evs["events"] if e["late"]]
    check("审计流中可见迟到事件 O-00", any(e["order_id"] == "O-00" for e in late), evs)
    status, wms = call("GET", "/v1/watermarks", "auditor")
    wm = wms["watermarks"][0]
    check("水位 closed_period = 2026-09-10（未被迟到事件回退）",
          wm["closed_period"] == "2026-09-10", wms)

    # ---- 7. 审计快照、差异与哈希链 ----
    print("-- 7. 时点快照、两次重算差异、哈希链")
    status, snap = call("GET", "/v1/audit/snapshot", "auditor")
    check("快照与账本对账一致", status == 200 and snap["match"] is True, snap)
    check("快照中创作者余额 827.50", snap["balances"].get("creator:li") == 82750, snap)
    demo_settlements = [s for s in snap["settlements"]
                        if s["period"] in ("2026-09-09", "2026-09-10")]
    check("快照只含 09-09 与 09-10 两笔结算（迟到的 O-00 未计入）",
          len(demo_settlements) == 2, snap["settlements"])

    conf_seqs = [e["seq"] for e in evs["events"] if e["type"] == "settlement_confirmation"]
    first_conf = min(conf_seqs)
    head = max(e["seq"] for e in evs["events"])
    status, diff = call("GET", f"/v1/audit/diff?from_seq={first_conf}&to_seq={head}", "auditor")
    check("两次重算差异：创作者增量 476.50",
          status == 200 and diff["deltas"].get("creator:li") == 47650, diff)
    check("差异列出新结算（09-10 周期）",
          any(s["period"] == "2026-09-10" for s in diff["new_settlements"]), diff)

    status, chain = call("GET", "/v1/audit/verify-chain", "auditor")
    check("哈希链校验通过（原始结算未被改写）",
          status == 200 and chain["ok"] is True, chain)

    # ---- 汇总 ----
    print()
    if FAILURES:
        print(f"!! {len(FAILURES)} 项断言失败: {FAILURES}")
        sys.exit(1)
    print("== 全部断言通过。关键数字：")
    print(f"   创作者 827.50 | 合作社 {cents(coop_total)} | 周期 09-09 v1 / 09-10 v2 | 冲正独立成腿")


if __name__ == "__main__":
    main()
