#!/usr/bin/env python3
"""Unit tests for tests/acceptance_linux_state.py (run: python3 tests/acceptance_linux_state_test.py -v)."""

import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HELPER = Path(__file__).resolve().parent / "acceptance_linux_state.py"

SCHEMA = """
CREATE TABLE projects (key TEXT PRIMARY KEY, data JSON NOT NULL);
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL,
    launch_pane_id TEXT NULL,
    public_pane_id TEXT NOT NULL DEFAULT '',
    data JSON NOT NULL
);
CREATE TABLE inbox (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    data JSON NOT NULL
);
CREATE TABLE ready_sessions (
    launch_pane_id TEXT PRIMARY KEY,
    omp_session_id TEXT NOT NULL DEFAULT '',
    omp_session_file TEXT NOT NULL DEFAULT '',
    omp_session_on_disk INTEGER NOT NULL DEFAULT 0,
    protocol INTEGER NOT NULL DEFAULT 1,
    verified INTEGER NOT NULL DEFAULT 0,
    seen_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tickets (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    data JSON NOT NULL
);
"""

TABLES = ("projects", "tasks", "inbox", "ready_sessions", "tickets")


def make_db(directory):
    db = os.path.join(directory, "orchestrator.sqlite")
    con = sqlite3.connect(db)
    con.executescript(SCHEMA)
    # Project A (this run): project row, task, inbox note, ticket, worker session.
    con.execute("INSERT INTO projects VALUES ('A', ?)", (json.dumps({"OMPSessionID": "sess-A"}),))
    con.execute("INSERT INTO tasks VALUES ('task-A', 'A', 'pane-A1', 'pub-A', ?)",
                (json.dumps({"OMPSessionID": "sess-A-task"}),))
    con.execute("INSERT INTO inbox VALUES ('note-A', 'A', 'task-A', ?)", (json.dumps({}),))
    con.execute("INSERT INTO tickets VALUES ('ticket-A', 'A', 1, ?)", (json.dumps({}),))
    con.execute("INSERT INTO ready_sessions VALUES ('pane-A1', 'sess-A-task', '/s/a.jsonl', 1, 1, 1, 10)")
    # Project B: nothing in it references project A.
    con.execute("INSERT INTO projects VALUES ('B', ?)", (json.dumps({"OMPSessionID": "sess-B"}),))
    con.execute("INSERT INTO tasks VALUES ('task-B', 'B', 'pane-B1', 'pub-B', ?)",
                (json.dumps({"OMPSessionID": "sess-B-task"}),))
    con.execute("INSERT INTO inbox VALUES ('note-B', 'B', 'task-B', ?)", (json.dumps({}),))
    con.execute("INSERT INTO tickets VALUES ('ticket-B', 'B', 1, ?)", (json.dumps({}),))
    con.execute("INSERT INTO ready_sessions VALUES ('pane-B1', 'sess-B-task', '/s/b.jsonl', 1, 1, 1, 10)")
    con.commit()
    con.close()
    return db


def run_helper(*args):
    proc = subprocess.run([sys.executable, str(HELPER), *args],
                          capture_output=True, text=True, timeout=60)
    return proc.returncode, proc.stdout, proc.stderr


def table_rows(db, table):
    con = sqlite3.connect(db)
    try:
        return sorted(con.execute("SELECT * FROM " + table).fetchall())
    finally:
        con.close()


def all_rows(db):
    return {t: table_rows(db, t) for t in TABLES}


class AcceptanceLinuxStateTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.db = make_db(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_delete_scoped_removes_only_this_project_rows(self):
        code, out, err = run_helper(self.db, "delete-scoped", "A", "pane-A1")
        self.assertEqual(code, 0, err)
        result = json.loads(out)
        self.assertTrue(result["ok"])
        self.assertTrue(result["others_identical"])
        self.assertEqual(result["ours_remaining"], 0)
        self.assertEqual([r[0] for r in table_rows(self.db, "projects")], ["B"])
        self.assertEqual([r[0] for r in table_rows(self.db, "tasks")], ["task-B"])
        self.assertEqual([r[0] for r in table_rows(self.db, "inbox")], ["note-B"])
        self.assertEqual([r[0] for r in table_rows(self.db, "tickets")], ["ticket-B"])
        self.assertEqual([r[0] for r in table_rows(self.db, "ready_sessions")], ["pane-B1"])

    def test_delete_scoped_refuses_when_a_foreign_row_references_the_run(self):
        con = sqlite3.connect(self.db)
        con.execute("UPDATE tasks SET data = ? WHERE id = 'task-B'",
                    (json.dumps({"OMPSessionID": "sess-A-task"}),))
        con.commit()
        con.close()
        before = all_rows(self.db)
        code, out, _ = run_helper(self.db, "delete-scoped", "A", "pane-A1")
        self.assertEqual(code, 3)
        result = json.loads(out)
        self.assertFalse(result["ok"])
        self.assertIn("tasks/task-B", result["foreign"])
        self.assertEqual(all_rows(self.db), before)

    def test_plan_scoped_writes_nothing(self):
        before = Path(self.db).read_bytes()
        code, out, err = run_helper(self.db, "plan-scoped", "A", "pane-A1")
        self.assertEqual(code, 0, err)
        self.assertTrue(json.loads(out)["ok"])
        self.assertEqual(Path(self.db).read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
