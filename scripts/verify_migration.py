"""校验迁移结果:结构、行数、关键字段。用法:python verify_migration.py <db 路径>"""
import sqlite3
import sys

path = sys.argv[1]
conn = sqlite3.connect(path)
cur = conn.cursor()
one = lambda sql: cur.execute(sql).fetchone()

print("integrity_check  :", one("PRAGMA integrity_check")[0])
print("foreign_key_check:", cur.execute("PRAGMA foreign_key_check").fetchall() or "OK")
print("journal_mode     :", one("PRAGMA journal_mode")[0])

tables = ["users", "settings", "channels", "agents", "monitors", "rounds",
          "results", "hourly_stats", "monitor_states"]
counts = {t: one(f"SELECT COUNT(*) FROM {t}")[0] for t in tables}
for t in tables:
    print(f"  {t:15s}: {counts[t]}")

print("results 缺 scheduled_at :", one("SELECT COUNT(*) FROM results WHERE scheduled_at=0")[0])
print("rounds 状态分布        :", cur.execute("SELECT state, COUNT(*) FROM rounds GROUP BY state").fetchall())
print("最近 3 轮              :", cur.execute(
    "SELECT id, state, success_rate FROM rounds ORDER BY scheduled_at DESC LIMIT 3").fetchall())
print("monitor_states         :", cur.execute(
    "SELECT id, alert_state, consecutive, last_round_state FROM monitor_states").fetchall())
print("agents                 :", cur.execute(
    "SELECT name, status FROM agents ORDER BY created_at").fetchall())
print("settings 密钥非空      :", one(
    "SELECT enrollment_key_hash <> '' , jwt_secret <> '' FROM settings")[:])
print("hourly 桶样例          :", cur.execute(
    "SELECT hour, rounds, valid, success FROM hourly_stats ORDER BY hour DESC LIMIT 3").fetchall())
conn.close()
