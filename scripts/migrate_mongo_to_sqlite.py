"""一次性搬迁:旧 MongoDB 库(uptimemesh)→ 新 SQLite 库。

只读 MongoDB、只写目标 SQLite 文件。主键(24 位 hex)与列语义一一对应,故 ID
原样保留;时间由 BSON Date 转 UTC 秒。唯一需要推导的是 results.scheduled_at
(迁移后新增的列):取所属轮次的 scheduledAt,与「小时分桶/窗口过滤以轮次计划
时间为准」的新口径一致。

用法:
    python migrate_mongo_to_sqlite.py --uri <mongo uri> --out <目标 .db 路径>
    python migrate_mongo_to_sqlite.py --uri ... --out ... --overwrite   # 覆盖已存在的目标

退出码 0 表示成功,并把各表行数打印成 JSON,便于比对。
"""

import argparse
import datetime as dt
import json
import os
import sqlite3
import sys

import pymongo

DB_NAME = "uptimemesh"


def unix_sec(value):
    """BSON Date / 数值 → UTC Unix 秒;缺失或零值 → 0。"""
    if value is None:
        return 0
    if isinstance(value, dt.datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=dt.timezone.utc)
        return int(value.timestamp())
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def json_or_null(value):
    """数组/结构化字段 → JSON 文本;空值 → NULL(与 store.mustJSON 一致)。"""
    if value in (None, "", [], {}):
        return None
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def text_or_empty(value):
    return "" if value is None else str(value)


def bool_int(value):
    return 1 if value else 0


def real_or_zero(value):
    return 0.0 if value is None else float(value)


def int_or_zero(value):
    return 0 if value is None else int(value)


def build_schema(conn):
    """建表:与 internal/store/schema.sql 保持一致(此处内联,避免依赖仓库路径)。"""
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS users (
          id TEXT PRIMARY KEY, username TEXT NOT NULL, pass_hash TEXT NOT NULL,
          created_at INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS settings (
          id TEXT PRIMARY KEY, enrollment_key_hash TEXT NOT NULL DEFAULT '',
          enrollment_key TEXT NOT NULL DEFAULT '', jwt_secret TEXT NOT NULL DEFAULT '',
          round_grace_seconds INTEGER NOT NULL DEFAULT 0,
          result_retention_days INTEGER NOT NULL DEFAULT 0,
          notify_templates TEXT, created_at INTEGER NOT NULL DEFAULT 0,
          updated_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS channels (
          id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL,
          body_template TEXT, created_at INTEGER NOT NULL DEFAULT 0,
          updated_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS agents (
          id TEXT PRIMARY KEY, name TEXT NOT NULL, version TEXT NOT NULL DEFAULT '',
          os TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '',
          source_ip TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
          credential_hash TEXT NOT NULL DEFAULT '', cred_plain TEXT,
          country TEXT NOT NULL DEFAULT '', region TEXT NOT NULL DEFAULT '',
          region_ip_cache TEXT NOT NULL DEFAULT '',
          last_seen INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
          updated_at INTEGER NOT NULL DEFAULT 0);
        CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_identity ON agents (name, source_ip);
        CREATE TABLE IF NOT EXISTS monitors (
          id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT 'http', name TEXT NOT NULL,
          group_name TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 0,
          period INTEGER NOT NULL DEFAULT 60, timeout INTEGER NOT NULL DEFAULT 0,
          threshold REAL NOT NULL DEFAULT 0, consecutive INTEGER NOT NULL DEFAULT 0,
          url TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', headers TEXT,
          body TEXT NOT NULL DEFAULT '', expect_status_specs TEXT,
          expect_status_codes TEXT, expect_contains TEXT, expect_not_contains TEXT,
          allow_insecure_tls INTEGER NOT NULL DEFAULT 0,
          target_host TEXT NOT NULL DEFAULT '', assign_mode TEXT NOT NULL DEFAULT '',
          assigned_agent_ids TEXT, excluded_agent_ids TEXT, channel_ids TEXT,
          created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS rounds (
          id TEXT PRIMARY KEY, monitor_id TEXT NOT NULL, assigned_agent_ids TEXT,
          scheduled_at INTEGER NOT NULL, deadline INTEGER NOT NULL DEFAULT 0,
          closed_at INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
          success INTEGER NOT NULL DEFAULT 0, valid INTEGER NOT NULL DEFAULT 0,
          total_agents INTEGER NOT NULL DEFAULT 0, success_rate REAL NOT NULL DEFAULT 0,
          latency_sum_ms REAL NOT NULL DEFAULT 0,
          latency_count INTEGER NOT NULL DEFAULT 0, missing_alive TEXT,
          missing_dead TEXT, created_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS results (
          id TEXT PRIMARY KEY, round_id TEXT NOT NULL, monitor_id TEXT NOT NULL,
          agent_id TEXT NOT NULL, ok INTEGER NOT NULL DEFAULT 0,
          latency_ms REAL NOT NULL DEFAULT 0, http_status INTEGER NOT NULL DEFAULT 0,
          error TEXT NOT NULL DEFAULT '', late INTEGER NOT NULL DEFAULT 0,
          scheduled_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS hourly_stats (
          id TEXT PRIMARY KEY, monitor_id TEXT NOT NULL, hour TEXT NOT NULL,
          rounds INTEGER NOT NULL DEFAULT 0, valid INTEGER NOT NULL DEFAULT 0,
          success INTEGER NOT NULL DEFAULT 0, latency_sum_ms REAL NOT NULL DEFAULT 0,
          latency_count INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
        CREATE TABLE IF NOT EXISTS monitor_states (
          id TEXT PRIMARY KEY, alert_state TEXT NOT NULL DEFAULT 'UP',
          consecutive INTEGER NOT NULL DEFAULT 0, last_round_state TEXT NOT NULL DEFAULT '',
          last_success_rate REAL NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
        """
    )


def main():
    parser = argparse.ArgumentParser(description="旧 MongoDB 库 → SQLite 一次性搬迁")
    parser.add_argument("--uri", required=True, help="源 MongoDB 连接串")
    parser.add_argument("--db", default=DB_NAME, help="源库名(默认 uptimemesh)")
    parser.add_argument("--out", required=True, help="目标 SQLite 文件路径")
    parser.add_argument("--overwrite", action="store_true",
                        help="目标文件已存在时先删除重建(默认拒绝,避免误覆盖)")
    args = parser.parse_args()

    if os.path.exists(args.out):
        if not args.overwrite:
            print(f"目标文件已存在,拒绝覆盖: {args.out}(需要时加 --overwrite)", file=sys.stderr)
            return 2
        os.remove(args.out)

    client = pymongo.MongoClient(args.uri, serverSelectionTimeoutMS=10000)
    src = client[args.db]
    # 先探活:源库连不上时在写任何东西之前失败。
    src.command("ping")
    conn = sqlite3.connect(args.out)
    build_schema(conn)
    cur = conn.cursor()
    counts = {}

    # users:主键固定为 admin。
    rows = list(src.users.find({}))
    for u in rows:
        cur.execute(
            "INSERT OR REPLACE INTO users (id, username, pass_hash, created_at) VALUES (?,?,?,?)",
            (text_or_empty(u.get("_id")), text_or_empty(u.get("username")),
             text_or_empty(u.get("passHash")), unix_sec(u.get("createdAt"))))
    counts["users"] = len(rows)

    # settings:保留接入密钥/JWT 密钥等,避免已有节点与登录态失效。
    rows = list(src.settings.find({}))
    for s in rows:
        cur.execute(
            """INSERT OR REPLACE INTO settings (id, enrollment_key_hash, enrollment_key,
               jwt_secret, round_grace_seconds, result_retention_days, notify_templates,
               created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(s.get("_id")), text_or_empty(s.get("enrollmentKeyHash")),
             text_or_empty(s.get("enrollmentKey")), text_or_empty(s.get("jwtSecret")),
             int_or_zero(s.get("roundGraceSeconds")), int_or_zero(s.get("resultRetentionDays")),
             json_or_null(s.get("notifyTemplates")),
             unix_sec(s.get("createdAt")), unix_sec(s.get("updatedAt"))))
    counts["settings"] = len(rows)

    rows = list(src.channels.find({}))
    for c in rows:
        cur.execute(
            """INSERT OR REPLACE INTO channels (id, name, url, body_template,
               created_at, updated_at) VALUES (?,?,?,?,?,?)""",
            (text_or_empty(c.get("_id")), text_or_empty(c.get("name")),
             text_or_empty(c.get("url")), c.get("bodyTemplate"),
             unix_sec(c.get("createdAt")), unix_sec(c.get("updatedAt"))))
    counts["channels"] = len(rows)

    rows = list(src.agents.find({}))
    for a in rows:
        cur.execute(
            """INSERT OR REPLACE INTO agents (id, name, version, os, arch, source_ip,
               status, credential_hash, cred_plain, country, region, region_ip_cache,
               last_seen, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(a.get("_id")), text_or_empty(a.get("name")),
             text_or_empty(a.get("version")), text_or_empty(a.get("os")),
             text_or_empty(a.get("arch")), text_or_empty(a.get("sourceIp")),
             text_or_empty(a.get("status")), text_or_empty(a.get("credentialHash")),
             a.get("credPlain"), text_or_empty(a.get("country")),
             text_or_empty(a.get("region")), text_or_empty(a.get("regionIpCache")),
             unix_sec(a.get("lastSeen")), unix_sec(a.get("createdAt")),
             unix_sec(a.get("updatedAt"))))
    counts["agents"] = len(rows)

    rows = list(src.monitors.find({}))
    for m in rows:
        cur.execute(
            """INSERT OR REPLACE INTO monitors (id, type, name, group_name, enabled,
               period, timeout, threshold, consecutive, url, method, headers, body,
               expect_status_specs, expect_status_codes, expect_contains,
               expect_not_contains, allow_insecure_tls, target_host, assign_mode,
               assigned_agent_ids, excluded_agent_ids, channel_ids, created_at,
               updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(m.get("_id")), text_or_empty(m.get("type")),
             text_or_empty(m.get("name")), text_or_empty(m.get("group")),
             bool_int(m.get("enabled")), int_or_zero(m.get("period")),
             int_or_zero(m.get("timeout")), real_or_zero(m.get("threshold")),
             int_or_zero(m.get("consecutive")), text_or_empty(m.get("url")),
             text_or_empty(m.get("method")), json_or_null(m.get("headers")),
             text_or_empty(m.get("body")), json_or_null(m.get("expectStatusSpecs")),
             json_or_null(m.get("expectStatusCodes")), json_or_null(m.get("expectContains")),
             json_or_null(m.get("expectNotContains")), bool_int(m.get("allowInsecureTLS")),
             text_or_empty(m.get("targetHost")), text_or_empty(m.get("assignMode")),
             json_or_null(m.get("assignedAgentIds")), json_or_null(m.get("excludedAgentIds")),
             json_or_null(m.get("channelIds")), unix_sec(m.get("createdAt")),
             unix_sec(m.get("updatedAt"))))
    counts["monitors"] = len(rows)

    rows = list(src.rounds.find({}))
    for r in rows:
        cur.execute(
            """INSERT OR REPLACE INTO rounds (id, monitor_id, assigned_agent_ids,
               scheduled_at, deadline, closed_at, state, success, valid, total_agents,
               success_rate, latency_sum_ms, latency_count, missing_alive, missing_dead,
               created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(r.get("_id")), text_or_empty(r.get("monitorId")),
             json_or_null(r.get("assignedAgentIds")), unix_sec(r.get("scheduledAt")),
             unix_sec(r.get("deadline")), unix_sec(r.get("closedAt")),
             text_or_empty(r.get("state")), int_or_zero(r.get("success")),
             int_or_zero(r.get("valid")), int_or_zero(r.get("totalAgents")),
             real_or_zero(r.get("successRate")), real_or_zero(r.get("latencySumMs")),
             int_or_zero(r.get("latencyCount")), json_or_null(r.get("missingAlive")),
             json_or_null(r.get("missingDead")), unix_sec(r.get("createdAt"))))
    counts["rounds"] = len(rows)

    # results:createdAt 保留原值;scheduled_at 从所属轮次推导(新列)。
    round_scheduled = {text_or_empty(r.get("_id")): unix_sec(r.get("scheduledAt"))
                       for r in rows}
    result_rows = list(src.results.find({}))
    for res in result_rows:
        round_id = text_or_empty(res.get("roundId"))
        cur.execute(
            """INSERT OR REPLACE INTO results (id, round_id, monitor_id, agent_id, ok,
               latency_ms, http_status, error, late, scheduled_at, created_at)
               VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(res.get("_id")), round_id,
             text_or_empty(res.get("monitorId")), text_or_empty(res.get("agentId")),
             bool_int(res.get("ok")), real_or_zero(res.get("latencyMs")),
             int_or_zero(res.get("httpStatus")), text_or_empty(res.get("error")),
             bool_int(res.get("late")), round_scheduled.get(round_id, 0),
             unix_sec(res.get("createdAt"))))
    counts["results"] = len(result_rows)

    rows = list(src.hourly_stats.find({}))
    for h in rows:
        # 旧库 hour 存 BSON Date;新库存 HourID() 的 YYYYMMDDHH(UTC)桶键。
        hour = h.get("hour")
        if isinstance(hour, dt.datetime):
            if hour.tzinfo is None:
                hour = hour.replace(tzinfo=dt.timezone.utc)
            bucket = hour.astimezone(dt.timezone.utc).strftime("%Y%m%d%H")
        else:
            bucket = text_or_empty(hour)
        cur.execute(
            """INSERT OR REPLACE INTO hourly_stats (id, monitor_id, hour, rounds, valid,
               success, latency_sum_ms, latency_count, updated_at)
               VALUES (?,?,?,?,?,?,?,?,?)""",
            (text_or_empty(h.get("_id")), text_or_empty(h.get("monitorId")), bucket,
             int_or_zero(h.get("rounds")), int_or_zero(h.get("valid")),
             int_or_zero(h.get("success")), real_or_zero(h.get("latencySumMs")),
             int_or_zero(h.get("latencyCount")), unix_sec(h.get("updatedAt"))))
    counts["hourly_stats"] = len(rows)

    rows = list(src.monitor_states.find({}))
    for s in rows:
        cur.execute(
            """INSERT OR REPLACE INTO monitor_states (id, alert_state, consecutive,
               last_round_state, last_success_rate, updated_at) VALUES (?,?,?,?,?,?)""",
            (text_or_empty(s.get("_id")), text_or_empty(s.get("alertState")),
             int_or_zero(s.get("consecutive")), text_or_empty(s.get("lastRoundState")),
             real_or_zero(s.get("lastSuccessRate")), unix_sec(s.get("updatedAt"))))
    counts["monitor_states"] = len(rows)

    conn.commit()
    print("migrated=" + json.dumps(counts, ensure_ascii=False))
    print("sqlite_check=" + json.dumps(
        {t: cur.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0] for t in counts},
        ensure_ascii=False))
    conn.close()
    client.close()


if __name__ == "__main__":
    main()
