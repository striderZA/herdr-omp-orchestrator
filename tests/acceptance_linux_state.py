#!/usr/bin/env python3
"""State helper for tests/acceptance_linux.sh.

Port of $script:StatePython in tests/acceptance_windows.ps1: the same operations,
JSON keys and exit codes.

  <db> dump                          print every table (read-only)
  <db> plan-scoped <key> <panes_csv> count this run's rows (read-only, writes nothing)
  <db> delete-scoped <key> <panes>   delete this run's rows; exit 3 and write nothing
                                     when a row of another project references the run
"""

import json
import sqlite3
import sys
import urllib.parse
from pathlib import Path

TABLES = ("projects", "tasks", "inbox", "ready_sessions", "tickets")


def fact(row):
    out = dict(row)
    data = out.get("data")
    if isinstance(data, str) and data:
        try:
            out["fact"] = json.loads(data)
        except Exception:
            out["fact"] = {}
    return out


def rows(cur, table):
    try:
        return [fact(r) for r in cur.execute("SELECT rowid AS _rowid, * FROM " + table)]
    except sqlite3.OperationalError as error:
        # A store written before the tickets table existed has no such table: that
        # is an empty tickets list, not a failure of the whole read. Any other
        # missing table means this is not the plugin's store, so it must fail.
        if table == "tickets" and "no such table" in str(error):
            return []
        raise


def identity(table, row):
    if table == "projects":
        return row.get("key")
    if table == "tasks":
        return row.get("id")
    if table == "inbox":
        return row.get("id")
    if table == "tickets":
        return row.get("id")
    return row.get("launch_pane_id")


def dump(cur):
    return {t: rows(cur, t) for t in TABLES}


def partitions(cur, key, panes):
    """Split every table into (all, ours, others) by exact identity."""
    data = dump(cur)
    ours = {}
    others = {}
    ours["projects"] = [r for r in data["projects"] if r.get("key") == key]
    others["projects"] = [r for r in data["projects"] if r.get("key") != key]
    ours["tasks"] = [r for r in data["tasks"] if r.get("project_key") == key]
    others["tasks"] = [r for r in data["tasks"] if r.get("project_key") != key]
    ours["inbox"] = [r for r in data["inbox"] if r.get("project_key") == key]
    others["inbox"] = [r for r in data["inbox"] if r.get("project_key") != key]
    ours["tickets"] = [r for r in data["tickets"] if r.get("project_key") == key]
    others["tickets"] = [r for r in data["tickets"] if r.get("project_key") != key]
    ours["ready_sessions"] = [r for r in data["ready_sessions"] if r.get("launch_pane_id") in panes]
    others["ready_sessions"] = [r for r in data["ready_sessions"] if r.get("launch_pane_id") not in panes]
    return data, ours, others


def canonical(table, rows_in):
    out = []
    for r in rows_in:
        out.append(str(identity(table, r)) + "|" + json.dumps(r, sort_keys=True, default=str))
    return sorted(out)


def foreign_reference(data, key):
    """Names of rows that are NOT this run's and reference one of its identities."""
    task_ids = set()
    session_ids = set()
    for t in data["tasks"]:
        if t.get("project_key") == key:
            task_ids.add(t.get("id"))
            session_ids.add((t.get("fact") or {}).get("OMPSessionID", ""))
    for p in data["projects"]:
        if p.get("key") == key:
            session_ids.add((p.get("fact") or {}).get("OMPSessionID", ""))
    session_ids.discard("")
    ticket_ids = {t.get("id") for t in data["tickets"] if t.get("project_key") == key}
    hits = []
    for t in data["tasks"]:
        if t.get("project_key") == key:
            continue
        f = t.get("fact") or {}
        if (t.get("id") in task_ids or f.get("OMPSessionID") in session_ids
                or (f.get("TicketID") and f.get("TicketID") in ticket_ids)):
            hits.append("tasks/" + str(t.get("id")))
    for p in data["projects"]:
        if p.get("key") == key:
            continue
        f = p.get("fact") or {}
        if f.get("OMPSessionID") in session_ids:
            hits.append("projects/" + str(p.get("key")))
    for i in data["inbox"]:
        if i.get("project_key") == key:
            continue
        if i.get("task_id") in task_ids:
            hits.append("inbox/" + str(i.get("id")))
    return hits


def open_read_only(db):
    uri = "file:" + urllib.parse.quote(str(Path(db).resolve())) + "?mode=ro"
    con = sqlite3.connect(uri, uri=True, timeout=10.0)
    con.row_factory = sqlite3.Row
    return con


def main(argv):
    if len(argv) < 3:
        print(json.dumps({"ok": False, "error": "usage: <db> dump|plan-scoped|delete-scoped <key> <panes>"}))
        return 2
    db, op = argv[1], argv[2]
    key = argv[3] if len(argv) > 3 else ""
    panes = [p for p in (argv[4].split(",") if len(argv) > 4 and argv[4] else []) if p]

    if op == "dump":
        con = open_read_only(db)
        print(json.dumps({"ok": True, "tables": dump(con.cursor())}))
        return 0
    if op == "plan-scoped":
        con = open_read_only(db)
        data, ours, _ = partitions(con.cursor(), key, panes)
        counts = {t: len(ours[t]) for t in TABLES}
        print(json.dumps({"ok": True, "counts": counts, "foreign": foreign_reference(data, key),
                          "ours": {t: [identity(t, r) for r in ours[t]] for t in TABLES}}))
        return 0
    if op != "delete-scoped":
        print(json.dumps({"ok": False, "error": "unknown op " + op}))
        return 2

    con = sqlite3.connect(db, timeout=10.0)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    data, ours, others = partitions(cur, key, panes)
    counts = {t: len(ours[t]) for t in TABLES}
    hits = foreign_reference(data, key)
    if hits:
        print(json.dumps({"ok": False, "error": "a row outside this run references one of its identities",
                          "foreign": hits, "counts": counts}))
        return 3
    others_before = {t: canonical(t, others[t]) for t in TABLES}
    cur.execute("BEGIN IMMEDIATE")
    for t in TABLES:
        for r in ours[t]:
            cur.execute("DELETE FROM " + t + " WHERE rowid = ?", (r["_rowid"],))
    con.commit()
    _, ours_after, others_after_rows = partitions(cur, key, panes)
    others_after = {t: canonical(t, others_after_rows[t]) for t in TABLES}
    identical = all(others_before[t] == others_after[t] for t in TABLES)
    print(json.dumps({"ok": True, "deleted": counts, "others_identical": identical,
                      "others_before": sum(len(others_before[t]) for t in TABLES),
                      "others_after": sum(len(others_after[t]) for t in TABLES),
                      "ours_remaining": sum(len(ours_after[t]) for t in TABLES)}))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
