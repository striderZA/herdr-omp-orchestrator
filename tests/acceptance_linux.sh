#!/usr/bin/env bash
# Linux smoke path for the orchestrator plugin (issue #83, Task 8).
#
# Prints one line per check: "PASS|FAIL|INFO <id> <what>".
# Exit 0 when every check passes, 1 on any FAIL, 2 when a precondition refuses the
# run before anything is created (L0).
#
# Usage: tests/acceptance_linux.sh [--keep-resources] [--skip-state-cleanup] [--model <provider/model[:thinking]>]
#   --model ''  keeps OMP's own default model. The default is anthropic/claude-haiku-5-5:max.

set -u

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
HELPER="$REPO/tests/acceptance_linux_state.py"
BIN="$REPO/build/linux-plugin/herdr-omp-orchestrator"
PLUGIN_ID=user.omp-orchestrator
DEFAULT_MODEL='anthropic/claude-haiku-5-5:max'
MODEL_PATTERN='^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._/-]*(:(off|minimal|low|medium|high|xhigh|max|auto))?$'
CALLER_KEYS=(HERDR_ENV HERDR_PANE_ID HERDR_WORKSPACE_ID HERDR_TAB_ID HERDR_PLUGIN_ID
  HERDR_PLUGIN_ROOT HERDR_PLUGIN_STATE_DIR HERDR_PLUGIN_CONFIG_DIR HERDR_PLUGIN_CONTEXT_JSON
  HERDR_PLUGIN_ACTION_ID HERDR_PLUGIN_EVENT HERDR_PLUGIN_EVENT_JSON HERDR_PLUGIN_ENTRYPOINT_ID
  HERDR_BIN_PATH HERDR_SOCKET_PATH HERDR_ORCH_HERDR_BIN_PATH HERDR_CONFIG_PATH)
UNSET=()
for key in "${CALLER_KEYS[@]}"; do UNSET+=(-u "$key"); done

KEEP=0
SKIP_STATE=0
MODEL=$DEFAULT_MODEL
FAILS=0
HERDR_BIN=""
OMP_BIN=""
HEX=""
SESSION=""
RUN=""
CONFIG=""
OVERLAY=""
PCF=""
STORE=""
STATE_DIR=""
SERVER_STARTED=0
SERVER_PID=""
SESSION_STARTED=0
PRIMARY=""
WS=""
TAB=""
TASK=""
WPANE=""
TICKET=""
LOG_ID=""
SETUP_LOG=""
RESTART_LOG=""
W=0
BRIDGE_OUT=""
DEFAULT_BEFORE=""

pass() { printf 'PASS %s %s\n' "$1" "$2"; }
fail() { printf 'FAIL %s %s\n' "$1" "$2"; FAILS=$((FAILS + 1)); }
info() { printf 'INFO %s %s\n' "$1" "$2"; }
refuse() { fail "$1" "$2"; exit 2; }

clean() { env "${UNSET[@]}" "$@"; }
hn() { clean "$HERDR_BIN" "$@"; }
hs() { clean "$HERDR_BIN" --session "$SESSION" "$@"; }
sessions_json() { hn session list --json; }

poll() {
  local seconds=$1
  shift
  local end=$((SECONDS + seconds))
  while :; do
    if "$@"; then return 0; fi
    if ((SECONDS >= end)); then return 1; fi
    sleep 1
  done
}

dump_store() { python3 "$HELPER" "$STORE" dump 2>/dev/null; }
ready_row() {
  dump_store | jq -c --arg p "$1" '[.tables.ready_sessions[]? | select(.launch_pane_id == $p)] | .[0] // empty'
}
task_launch_pane() {
  dump_store | jq -r --arg t "$1" '[.tables.tasks[]? | select(.id == $t) | .launch_pane_id // empty] | .[0] // empty'
}
socket_path() {
  sessions_json | jq -r --arg s "$SESSION" '[.sessions[]? | select(.name == $s)] | .[0].socket_path // empty'
}

server_running() {
  sessions_json | jq -e --arg s "$SESSION" '[.sessions[]? | select(.name == $s and .running == true)] | length > 0' >/dev/null
}
server_stopped() {
  sessions_json | jq -e --arg s "$SESSION" '[.sessions[]? | select(.name == $s and .running == true)] | length == 0' >/dev/null
}

focus_projection() {
  clean "$HERDR_BIN" api snapshot 2>/dev/null |
    jq -cS '(.result.snapshot // .snapshot) | {focused_workspace_id, focused_tab_id, focused_pane_id}'
}
default_running() {
  sessions_json | jq -e '[.sessions[]? | select(.name == "default" and .running == true)] | length > 0' >/dev/null
}

# bridge_as <launch_pane> <method> <params-json>
# Leaves the answer in BRIDGE_OUT. Returns the bridge's exit status (2 is a bridge error).
bridge_as() {
  local pane=$1 method=$2 params=$3 row sid sf od sock body rc
  BRIDGE_OUT='{"ok":false,"error":{"code":"no_handshake"}}'
  row=$(ready_row "$pane")
  if [[ -z $row ]]; then return 0; fi
  sid=$(jq -r '.omp_session_id' <<<"$row")
  sf=$(jq -r '.omp_session_file' <<<"$row")
  od=$(jq -r '.omp_session_on_disk' <<<"$row")
  body=$(jq -cn --arg m "$method" --arg sid "$sid" --arg sf "$sf" --argjson od "$od" --argjson p "$params" \
    '{version:1, method:$m, ompSessionID:$sid, ompSessionFile:$sf, ompSessionOnDisk:($od == 1), params:$p}') || return 2
  sock=$(socket_path)
  BRIDGE_OUT=$(cd "$REPO" && printf '%s' "$body" | timeout 180 env "${UNSET[@]}" \
    HERDR_ENV=1 HERDR_PANE_ID="$pane" HERDR_SOCKET_PATH="$sock" HERDR_PLUGIN_STATE_DIR="$STATE_DIR" \
    HERDR_ORCH_HERDR_BIN_PATH="$HERDR_BIN" "$BIN" bridge "$method")
  rc=$?
  return "$rc"
}

bridge_ok() { jq -e '.ok == true' >/dev/null 2>&1 <<<"$BRIDGE_OUT"; }

primary_resolves() {
  bridge_as "$PRIMARY" resolve '{}' && bridge_ok
}

# ---------------------------------------------------------------------------
# L0 preflight: nothing is created until every precondition holds.
# ---------------------------------------------------------------------------
l0_preflight() {
  local tool missing=() sj entry root want
  for tool in herdr omp git jq python3 setsid; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  ((${#missing[@]} == 0)) || refuse L0.tools "not on PATH: ${missing[*]}"
  HERDR_BIN=$(command -v herdr)
  OMP_BIN=$(command -v omp)
  pass L0.tools "herdr, omp, git, jq, python3 and setsid are on PATH"

  [[ -x $BIN ]] || refuse L0.binary "missing $BIN; build build/linux-plugin/herdr-omp-orchestrator first"
  pass L0.binary "plugin binary exists"

  local plist
  plist=$(hn plugin list --json --plugin "$PLUGIN_ID") || refuse L0.plugin "herdr plugin list --json failed"
  entry=$(jq -c --arg id "$PLUGIN_ID" '[.result.plugins[]? | select(.plugin_id == $id)] | if length == 1 then .[0] else empty end' <<<"$plist")
  [[ -n $entry ]] || refuse L0.plugin "$PLUGIN_ID is not linked exactly once; link it first (this run never links)"
  root=$(jq -r '.plugin_root // ""' <<<"$entry")
  want=$(realpath -m "$REPO/build/linux-plugin")
  [[ -n $root && $(realpath -m "$root") == "$want" ]] || refuse L0.root "plugin_root '$root' is not $want"
  pass L0.root "plugin_root is $want"
  jq -e '.enabled == true' >/dev/null <<<"$entry" || refuse L0.enabled "$PLUGIN_ID is not enabled"
  pass L0.enabled "$PLUGIN_ID is enabled"
  jq -e '(.warnings // []) | length == 0' >/dev/null <<<"$entry" || refuse L0.warnings "$PLUGIN_ID links with warnings"
  pass L0.warnings "$PLUGIN_ID links with no warnings"

  jq -e '[.actions[]? | select(.id == "setup")] | length == 1' >/dev/null <<<"$entry" ||
    refuse L0.setup "$PLUGIN_ID declares no setup action"
  pass L0.setup "$PLUGIN_ID declares a setup action"

  sj=$(sessions_json) || refuse L0.sessions "herdr session list --json failed"
  jq -e '[.sessions[]? | select((.name | startswith("orch-acc-linux-")) and .running == true)] | length == 0' \
    >/dev/null <<<"$sj" || refuse L0.sessions "an orch-acc-linux-* session is already running"
  pass L0.sessions "no orch-acc-linux-* session is running"
}

# ---------------------------------------------------------------------------
# Run setup: config, model overlay, config path list, isolated server.
# ---------------------------------------------------------------------------
prepare_run() {
  HEX=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
  SESSION="orch-acc-linux-$HEX"
  RUN=$(mktemp -d) || return 1
  mkdir -p "$RUN/worktrees" "$RUN/repo"
  CONFIG="$RUN/herdr-config.toml"
  printf '[worktrees]\ndirectory = "%s"\n' "$RUN/worktrees" >"$CONFIG"
  OVERLAY="$RUN/omp-model-overlay.yml"
  if [[ -n $MODEL ]]; then
    printf 'modelRoles:\n  default: "%s"\n' "$MODEL" >"$OVERLAY"
    local entry files=() existing=()
    IFS=: read -ra existing <<<"${PI_CONFIG_FILES:-}"
    for entry in "${existing[@]}"; do
      [[ -z ${entry//[[:space:]]/} || $entry == "$OVERLAY" ]] && continue
      local seen=0 f
      for f in "${files[@]}"; do [[ $f == "$entry" ]] && seen=1; done
      ((seen)) || files+=("$entry")
    done
    files+=("$OVERLAY")
    PCF=$(IFS=:; printf '%s' "${files[*]}")
  fi
  STORE=""
}

start_isolated_server() {
  local seconds=$1 envs=(HERDR_CONFIG_PATH="$CONFIG")
  [[ -n $MODEL ]] && envs+=(PI_CONFIG_FILES="$PCF")
  SERVER_STARTED=1
  clean setsid env "${envs[@]}" "$HERDR_BIN" --session "$SESSION" server >"$RUN/server.log" 2>&1 </dev/null &
  SERVER_PID=$!
  if poll "$seconds" server_running; then
    SESSION_STARTED=1
    return 0
  fi
  return 1
}

# ---------------------------------------------------------------------------
# L1 setup
# ---------------------------------------------------------------------------
log_entry() {
  hs plugin log list --plugin "$PLUGIN_ID" --limit 100 2>/dev/null |
    jq -c --arg id "$1" '[.result.logs[]? | select(.log_id == $id)] | .[0] // empty'
}
setup_log_done() {
  SETUP_LOG=$(log_entry "$LOG_ID")
  [[ -n $SETUP_LOG && $(jq -r '.status // ""' <<<"$SETUP_LOG") != running ]]
}

l1_setup() {
  local out rc stdout ext raw line
  out=$(hs plugin action invoke setup --plugin "$PLUGIN_ID" 2>"$RUN/setup.err")
  rc=$?
  LOG_ID=$(jq -r '.result.log.log_id // empty' <<<"$out" 2>/dev/null)
  if ((rc != 0)) || [[ -z $LOG_ID ]]; then
    fail L1.invoke "setup action invoke failed (exit $rc)"
    return 1
  fi
  if ! poll 120 setup_log_done; then
    fail L1.invoke "setup log $LOG_ID still running after 120 s"
    return 1
  fi
  stdout=$(jq -r '.stdout // ""' <<<"$SETUP_LOG")
  if [[ $stdout != *'"ok":true'* ]]; then
    fail L1.invoke "setup did not report ok"
    return 1
  fi
  pass L1.invoke "setup ran and reported ok"

  ext=$(jq -Rr 'fromjson? | .extension? // empty' <<<"$stdout" | head -n1)
  if [[ -z $ext || ! -f $ext ]]; then
    fail L1.extension "setup answer names no installed extension ('$ext')"
    return 1
  fi
  line=$(grep -m1 '^const PLUGIN_STATE_DIR = ' "$ext")
  if [[ $line =~ ^const\ PLUGIN_STATE_DIR\ =\ \"(.*)\"\;$ ]]; then
    raw=${BASH_REMATCH[1]}
    STATE_DIR=$(jq -rn --argjson v "\"$raw\"" '$v')
  fi
  if [[ -z $STATE_DIR ]]; then
    fail L1.state-dir "no PLUGIN_STATE_DIR constant in $ext"
    return 1
  fi
  STORE="$STATE_DIR/orchestrator.sqlite"
  pass L1.state-dir "store is $STORE"
}

# ---------------------------------------------------------------------------
# L2 bind
# ---------------------------------------------------------------------------
l2_bind() {
  local created rc
  if ! git init -q "$RUN/repo" ||
    ! git -C "$RUN/repo" -c user.name=acceptance -c user.email=acceptance@example.invalid \
      commit -q --allow-empty -m "acceptance base"; then
    fail L2.repo "could not create the temporary repository"
    return 1
  fi
  created=$(hs workspace create --cwd "$RUN/repo" --no-focus --label "accept-$HEX" 2>/dev/null) || {
    fail L2.workspace "workspace create failed"
    return 1
  }
  WS=$(jq -r '.result.workspace.workspace_id // empty' <<<"$created")
  PRIMARY=$(jq -r '.result.root_pane.pane_id // empty' <<<"$created")
  TAB=$(jq -r '.result.tab.tab_id // empty' <<<"$created")
  local cwd
  cwd=$(jq -r '.result.root_pane.cwd // empty' <<<"$created")
  if [[ -z $WS || -z $PRIMARY || -z $TAB ]]; then
    fail L2.workspace "workspace create returned no ids"
    return 1
  fi
  if [[ $(realpath -m "$cwd") != "$(realpath -m "$RUN/repo")" ]]; then
    fail L2.workspace "root pane cwd '$cwd' is not the temporary repository"
    return 1
  fi
  pass L2.workspace "workspace $WS, primary pane $PRIMARY in the temporary repository"

  clean env HERDR_ENV=1 HERDR_PANE_ID="$PRIMARY" HERDR_WORKSPACE_ID="$WS" HERDR_TAB_ID="$TAB" \
    "$HERDR_BIN" --session "$SESSION" plugin action invoke bind --plugin "$PLUGIN_ID" >/dev/null 2>"$RUN/bind.err"
  rc=$?
  if ((rc != 0)); then
    fail L2.bind "plugin action invoke bind failed (exit $rc)"
    return 1
  fi
  if ! poll 180 primary_resolves; then
    fail L2.bind "primary bridge resolve did not answer ok within 180 s"
    return 1
  fi
  pass L2.bind "bind ran and the primary answers resolve ok"
}

# ---------------------------------------------------------------------------
# L3 dispatch
# ---------------------------------------------------------------------------
l3_dispatch() {
  local base params rc wt
  base=$(git -C "$RUN/repo" rev-parse HEAD) || {
    fail L3.dispatch "could not read HEAD"
    return 1
  }
  params=$(jq -cn --arg b "$base" '{
    title: "ACCEPT LINUX: helper in a.txt",
    brief: "Create a.txt containing the word helper.",
    checks: ["git log -1 --format=%H"],
    dependencies: [],
    reservedPaths: ["a.txt"],
    baseRevision: $b
  }')
  bridge_as "$PRIMARY" dispatch "$params"
  rc=$?
  if ((rc != 0)) || ! bridge_ok; then
    fail L3.dispatch "dispatch was not answered ok: $BRIDGE_OUT"
    return 1
  fi
  TASK=$(jq -r '.result.taskId // empty' <<<"$BRIDGE_OUT")
  wt=$(jq -r '.result.worktreePath // empty' <<<"$BRIDGE_OUT")
  if [[ -z $TASK ]]; then
    fail L3.dispatch "dispatch answer has no taskId"
    return 1
  fi
  case "$(realpath -m "$wt")" in
    "$(realpath -m "$RUN/worktrees")"/*) pass L3.dispatch "task $TASK dispatched into $wt" ;;
    *) fail L3.dispatch "worktreePath '$wt' is not under $RUN/worktrees"; return 1 ;;
  esac
}

# ---------------------------------------------------------------------------
# L4 worker
# ---------------------------------------------------------------------------
worker_pane_ready() {
  WPANE=$(task_launch_pane "$TASK")
  [[ -n $WPANE ]]
}
worker_settled() {
  local st
  st=$(hs agent get "$WPANE" 2>/dev/null | jq -r '.result.agent.agent_status // empty')
  [[ $st == idle || $st == done ]]
}

l4_worker() {
  local verified
  if ! poll 180 worker_pane_ready; then
    fail L4.launch "dispatched task $TASK never received a launch pane"
    return 1
  fi
  pass L4.launch "worker launch pane is $WPANE"
  if ! poll 300 worker_settled; then
    fail L4.settle "worker pane $WPANE did not reach idle or done within 300 s"
    return 1
  fi
  pass L4.settle "worker pane $WPANE settled"
  verified=$(ready_row "$WPANE" | jq -r '.verified // 0')
  if [[ $verified == 1 ]]; then
    pass L4.verified "worker session is verified"
  else
    fail L4.verified "worker session verified=$verified"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# L5 ticket
# ---------------------------------------------------------------------------
planner_listed() {
  bridge_as "$PRIMARY" task_list '{}' && bridge_ok &&
    jq -e --arg t "$TICKET" '[.result.tasks[]? | select(.ticketId == $t)] | length > 0' >/dev/null <<<"$BRIDGE_OUT"
}

l5_ticket() {
  local digest move_params rc column
  bridge_as "$PRIMARY" ticket_create '{"title":"ACCEPT LINUX TICKET","body":"Created by tests/acceptance_linux.sh."}'
  rc=$?
  TICKET=$(jq -r '.result.ticketId // empty' <<<"$BRIDGE_OUT")
  if ((rc != 0)) || [[ -z $TICKET ]]; then
    fail L5.create "ticket_create returned no ticketId: $BRIDGE_OUT"
    return 1
  fi
  pass L5.create "ticket $TICKET created"

  bridge_as "$PRIMARY" ticket_move_preview "$(jq -cn --arg t "$TICKET" '{ticketId: $t}')"
  digest=$(jq -r '.result.digest // empty' <<<"$BRIDGE_OUT")
  if [[ -z $digest ]]; then
    fail L5.preview "ticket_move_preview returned no digest"
    return 1
  fi
  pass L5.preview "move preview returned a digest"

  move_params=$(jq -cn --arg t "$TICKET" --arg d "$digest" '{ticketId: $t, digest: $d}')
  bridge_as "$PRIMARY" ticket_move "$move_params"
  rc=$?
  if ((rc != 0)) || ! bridge_ok || [[ -n $(jq -r '.result.plannerNotStarted // empty' <<<"$BRIDGE_OUT") ]]; then
    fail L5.move "ticket_move was not ok without plannerNotStarted: $BRIDGE_OUT"
    return 1
  fi
  pass L5.move "ticket moved and its planner started"

  bridge_as "$PRIMARY" ticket_list '{}'
  column=$(jq -r --arg t "$TICKET" '[.result.tickets[]? | select(.ticketId == $t) | .column] | .[0] // empty' <<<"$BRIDGE_OUT")
  if [[ $column == todo ]]; then
    pass L5.list "ticket_list shows $TICKET in todo"
  else
    fail L5.list "ticket_list shows column '$column' for $TICKET"
    return 1
  fi

  if poll 120 planner_listed; then
    pass L5.planner "a planner task linked to the ticket appears in task_list"
  else
    fail L5.planner "no task linked to $TICKET appeared in task_list within 120 s"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# L6 restart
# ---------------------------------------------------------------------------
restart_log_done() {
  local entry
  entry=$(hs plugin log list --plugin "$PLUGIN_ID" --limit 50 2>/dev/null |
    jq -c --argjson w "$W" '[.result.logs[]? | select(.started_unix_ms > $w)] | sort_by(.started_unix_ms) | last // empty')
  [[ -n $entry && $(jq -r '.status // ""' <<<"$entry") != running ]] || return 1
  RESTART_LOG=$entry
}

l6_restart() {
  local stdout
  W=$(date +%s%3N)
  if ! hs server stop >/dev/null 2>&1; then
    fail L6.stop "herdr server stop failed"
    return 1
  fi
  if ! poll 60 server_stopped; then
    fail L6.stop "session still running 60 s after server stop"
    return 1
  fi
  pass L6.stop "isolated server stopped"

  if ! start_isolated_server 90; then
    fail L6.start "isolated server did not come back within 90 s"
    return 1
  fi
  pass L6.start "isolated server is running again"

  if ! poll 120 restart_log_done; then
    fail L6.startup "no startup hook entry finished after $W within 120 s"
    return 1
  fi
  stdout=$(jq -r '.stdout // ""' <<<"$RESTART_LOG")
  if [[ $stdout == *'"ok":true'* ]]; then
    pass L6.startup "startup hook reported ok after the restart"
  else
    fail L6.startup "startup hook did not report ok"
    return 1
  fi

  if ! poll 180 primary_resolves; then
    fail L6.resolve "primary resolve did not answer ok after the restart"
    return 1
  fi
  pass L6.resolve "primary resolve answers ok after the restart"

  bridge_as "$PRIMARY" task_list '{}'
  if bridge_ok && jq -e --arg t "$TASK" '[.result.tasks[]? | select(.taskId == $t)] | length > 0' >/dev/null <<<"$BRIDGE_OUT" &&
    [[ $(task_launch_pane "$TASK") == "$WPANE" ]]; then
    pass L6.tasks "task $TASK and its worker pane $WPANE survive the restart"
  else
    fail L6.tasks "task $TASK or its worker pane $WPANE changed across the restart"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# L7 model
# ---------------------------------------------------------------------------
transcript_pairs() {
  jq -R -r 'fromjson? | select(.type == "message" and .message.role == "assistant"
      and (.message.provider // "") != "" and (.message.model // "") != "")
    | "\(.message.provider)/\(.message.model)"' "$1" | sort -u
}

l7_model() {
  local resolved expected pane row file pairs mismatch p
  if [[ -z $MODEL ]]; then
    info L7 "no model pinned; every session uses the default of the OMP profile"
    return 0
  fi
  resolved=$(clean env PI_CONFIG_FILES="$PCF" "$OMP_BIN" config get modelRoles --json 2>/dev/null |
    jq -r '.value.default // empty')
  if [[ $resolved == "$MODEL" ]]; then
    pass L7.overlay "OMP resolves the pinned default model $MODEL"
  else
    fail L7.overlay "OMP resolved '$resolved', not the pinned '$MODEL'"
  fi

  expected=$MODEL
  for p in off minimal low medium high xhigh max auto; do expected=${expected%":$p"}; done
  for pane in "$PRIMARY" "$WPANE"; do
    row=$(ready_row "$pane")
    file=$(jq -r '.omp_session_file // ""' <<<"$row")
    if [[ -z $file || ! -r $file ]]; then
      fail L7.transcript "transcript of pane $pane is unreadable: '$file'"
      continue
    fi
    pairs=$(transcript_pairs "$file")
    if [[ -z $pairs ]]; then
      fail L7.transcript "transcript of pane $pane has no assistant turns"
      continue
    fi
    mismatch=0
    while IFS= read -r p; do [[ $p == "$expected" ]] || mismatch=1; done <<<"$pairs"
    if ((mismatch)); then
      fail L7.transcript "pane $pane ran on other models: $(tr '\n' ' ' <<<"$pairs")"
    else
      pass L7.transcript "every assistant turn of pane $pane used $expected"
    fi
  done
}

# ---------------------------------------------------------------------------
# L8 cleanup
# ---------------------------------------------------------------------------
l8_cleanup() {
  local key panes del rc cmd
  if ((SERVER_STARTED == 1 && KEEP == 0)); then
    hs server stop >/dev/null 2>&1
    if poll 60 server_stopped; then
      pass L8.stop "isolated server stopped"
    else
      fail L8.stop "isolated session still running 60 s after server stop"
    fi
  fi

  if ((KEEP == 0 && SKIP_STATE == 0)); then
    if [[ -n $PRIMARY && -n $STORE && -f $STORE ]]; then
      key=$(dump_store | jq -r --arg p "$PRIMARY" '[.tables.projects[]? | select(.fact.LaunchPaneID == $p) | .key] | .[0] // empty')
    else
      key=""
    fi
    if [[ -z $key ]]; then
      pass L8.state "no project was bound, so no state rows needed deleting"
    else
      panes=$(dump_store | jq -r --arg p "$PRIMARY" --arg k "$key" \
        '[$p] + [.tables.tasks[]? | select(.project_key == $k) | .launch_pane_id // empty] | unique | join(",")')
      del=$(python3 "$HELPER" "$STORE" delete-scoped "$key" "$panes" 2>/dev/null)
      rc=$?
      if ((rc == 0)) && jq -e '.ok == true and .others_identical == true and .ours_remaining == 0' >/dev/null <<<"$del"; then
        pass L8.state "this run's rows are deleted and every other row is identical"
      else
        fail L8.state "state cleanup exit $rc: $del"
      fi
    fi
  else
    info L8.state "state rows kept (--keep-resources or --skip-state-cleanup)"
  fi

  if ((KEEP == 0 && SESSION_STARTED == 1)); then
    if hn session delete "$SESSION" >/dev/null 2>&1 &&
      sessions_json | jq -e --arg s "$SESSION" '[.sessions[]? | select(.name == $s)] | length == 0' >/dev/null; then
      pass L8.session "isolated session is deleted"
    else
      fail L8.session "isolated session could not be deleted"
    fi
    if [[ -n $SERVER_PID ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
      cmd=$(tr '\0' ' ' </proc/"$SERVER_PID"/cmdline 2>/dev/null)
      if [[ $cmd == *"--session $SESSION"* ]]; then
        kill -TERM "$SERVER_PID" 2>/dev/null
        pass L8.server-process "sent TERM to the isolated server process $SERVER_PID"
      fi
    fi
  elif ((KEEP == 1)); then
    info L8.session "isolated session kept (--keep-resources)"
  fi

  if ((KEEP == 0)) && [[ -n $RUN ]]; then
    case $RUN in
      "${TMPDIR:-/tmp}"/tmp.*)
        if rm -rf -- "$RUN"; then pass L8.run "run root removed"; else fail L8.run "could not remove $RUN"; fi
        ;;
      *) fail L8.run "refusing to remove $RUN: outside ${TMPDIR:-/tmp}" ;;
    esac
  elif [[ -n $RUN ]]; then
    info L8.run "run root kept at $RUN (--keep-resources)"
  fi
}

default_session_check() {
  local after
  if [[ -z $DEFAULT_BEFORE ]]; then
    info default-session "not compared: the default session's server was not running before the run"
    return 0
  fi
  after=$(focus_projection)
  if [[ $after == "$DEFAULT_BEFORE" ]]; then
    info default-session "focus projection of the default session is unchanged"
  else
    fail default-session "focus projection changed: before $DEFAULT_BEFORE after $after"
  fi
}

main() {
  while (($#)); do
    case $1 in
      --keep-resources) KEEP=1 ;;
      --skip-state-cleanup) SKIP_STATE=1 ;;
      --model)
        (($# >= 2)) || refuse L0.args "--model needs a value"
        MODEL=$2
        shift
        ;;
      --model=*) MODEL=${1#--model=} ;;
      *) refuse L0.args "unknown argument: $1" ;;
    esac
    shift
  done
  if [[ -n $MODEL && ! $MODEL =~ $MODEL_PATTERN ]]; then
    refuse L0.args "--model must be spelled provider/model[:thinking], got '$MODEL'"
  fi

  l0_preflight

  if default_running; then
    DEFAULT_BEFORE=$(focus_projection)
  fi

  prepare_run || refuse L0.run "could not create the run root"

  if start_isolated_server 60 && pass L1.server "isolated server $SESSION is running" &&
    l1_setup && l2_bind && l3_dispatch && l4_worker && l5_ticket && l6_restart && l7_model; then
    :
  else
    [[ $SESSION_STARTED == 1 ]] || fail L1.server "isolated server did not start within 60 s"
  fi

  l8_cleanup
  default_session_check

  if ((FAILS == 0)); then exit 0; fi
  exit 1
}

main "$@"
