#!/usr/bin/env bash
# tools: cp dirname jq ln mkdir mktemp realpath rm sleep
#
# Installs this checkout as the OMP Orchestrator Herdr plugin on Linux. It is the
# Linux counterpart of install.ps1 and runs the same steps in the same order,
# stopping at the first one that fails:
#
#   Preflight  Finds herdr, go and jq on PATH and reads the plugin's current
#              Herdr link. If Herdr links the plugin from another folder, it
#              stops here, before anything is built or written.
#   [1/4]      Copies linux/herdr-plugin.toml into build/linux-plugin/, links
#              omp/ there (setup reads its extension source from the plugin
#              root, as on Windows), then builds herdr-omp-orchestrator there
#              with CGO_ENABLED=0.
#   [2/4]      Checks Herdr's OMP integration. It runs
#              `herdr integration install omp` only when
#              `herdr integration status` reports omp as not installed.
#   [3/4]      Links build/linux-plugin as a Herdr plugin, enables it if it is
#              disabled, and checks that Herdr reports no manifest warnings
#              and lists the setup action.
#   [4/4]      Runs the plugin's setup action, which writes the OMP extension
#              for the active profile, then reads the answer from Herdr's
#              plugin log.
#
# Every step is safe to repeat. The script never unlinks or uninstalls the
# plugin and never writes Herdr's integration file. It takes no arguments and
# installs the checkout that holds this file, whatever folder it is started from.
#
# Commands this script runs, besides herdr, go and uname, are listed in the
# `# tools:` line above. The install tests link exactly those commands.

set -euo pipefail

PLUGIN_ID='user.omp-orchestrator'
EXECUTABLE_NAME='herdr-omp-orchestrator'
SETUP_LOG_TIMEOUT_SECONDS=120
REPO_ROOT=$(cd "$(dirname "$0")" && pwd)
BUILD_DIR="$REPO_ROOT/build/linux-plugin"

HERDR=''
GO=''
REGISTRATION=''
INTEGRATION_LINE=''
LOG_ENTRY=''
RUN_CODE=0
RUN_OUT=''
RUN_ERR=''

fail() {
  printf 'install failed: %s\n' "$1" >&2
  exit 1
}

# Trims captured output for an error message and keeps it short.
detail() {
  local text=$1
  if (( ${#text} > 800 )); then text="${text:0:800}..."; fi
  printf '%s' "$text"
}

# Runs one command from an argv array, never through a shell, and captures its
# stdout and stderr apart. Sets RUN_CODE, RUN_OUT and RUN_ERR.
run() {
  local errfile
  errfile=$(mktemp)
  RUN_CODE=0
  RUN_OUT=$("$@" 2>"$errfile") || RUN_CODE=$?
  RUN_ERR=$(<"$errfile")
  rm -f -- "$errfile"
}

is_json() {
  [[ -n ${1//[[:space:]]/} ]] && jq -e . <<<"$1" >/dev/null 2>&1
}

require_json() {
  is_json "$1" || fail "$2 printed no JSON answer: $(detail "$1")"
}

# Herdr's JSON commands print one object shaped {"id": ..., "result": {...}}.
reg_get() {
  jq -r "$@" <<<"$REGISTRATION"
}

reg_json() {
  jq -c "$@" <<<"$REGISTRATION"
}

# Two paths name one folder when both resolve to the same place. A symlink to
# the folder counts. An empty path never names a folder.
same_folder() {
  [[ -n $1 && -n $2 ]] && [[ $(realpath -m -- "$1") == "$(realpath -m -- "$2")" ]]
}

find_tools() {
  HERDR=$(command -v herdr) || fail 'herdr was not found on PATH. Install Herdr 0.9.0 or newer, open a new terminal, and run this script again.'
  GO=$(command -v go) || fail 'go was not found on PATH. Install Go 1.27.1 (the version go.mod names), open a new terminal, and run this script again.'
  command -v jq >/dev/null || fail "jq was not found on PATH. Install it with 'sudo apt install jq', open a new terminal, and run this script again."
}

# This plugin's entry in `herdr plugin list --json`, or none when Herdr holds no
# link for it. Read-only. Sets REGISTRATION.
get_registration() {
  local matches count
  run "$HERDR" plugin list --json
  (( RUN_CODE == 0 )) || fail "herdr plugin list failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
  require_json "$RUN_OUT" 'herdr plugin list'
  matches=$(jq -c --arg id "$PLUGIN_ID" '[.result.plugins[]? | select(.plugin_id == $id)]' <<<"$RUN_OUT")
  count=$(jq 'length' <<<"$matches")
  (( count <= 1 )) || fail "herdr plugin list shows $PLUGIN_ID $count times"
  REGISTRATION=''
  if (( count == 1 )); then REGISTRATION=$(jq -c '.[0]' <<<"$matches"); fi
}

# Herdr keeps one link per plugin id. A link to another folder belongs to the
# user to release, so this script refuses rather than re-pointing it.
assert_no_other_link() {
  local linked
  if [[ -z $REGISTRATION ]]; then return 0; fi
  linked=$(reg_get '.plugin_root // ""')
  if same_folder "$linked" "$BUILD_DIR"; then return 0; fi
  fail "Herdr already links $PLUGIN_ID from '$linked', not from this build folder '$BUILD_DIR'. If that folder is gone or this checkout moved, run 'herdr plugin unlink $PLUGIN_ID' and then run this script again."
}

preflight() {
  local os
  os=$(uname -s)
  [[ $os == Linux ]] || fail "install.sh supports Linux only; this host reports '$os'. Run install.ps1 on Windows."
  find_tools
  [[ -f "$REPO_ROOT/linux/herdr-plugin.toml" ]] || fail "linux/herdr-plugin.toml is not in '$REPO_ROOT'. Run this script from the plugin repository."
  echo "Herdr: $HERDR"
  echo "Go:    $GO"
  get_registration
  assert_no_other_link
}

build_plugin() {
  local rc=0 out hint=''
  echo "[1/4] Building $EXECUTABLE_NAME (CGO_ENABLED=0)"
  mkdir -p "$BUILD_DIR"
  cp "$REPO_ROOT/linux/herdr-plugin.toml" "$BUILD_DIR/herdr-plugin.toml"
  ln -sfn -- "$REPO_ROOT/omp" "$BUILD_DIR/omp"
  out=$(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$BUILD_DIR/$EXECUTABLE_NAME" ./cmd/herdr-omp-orchestrator 2>&1) || rc=$?
  if (( rc != 0 )); then
    if [[ $out == *'text file busy'* ]]; then
      hint=" $EXECUTABLE_NAME is in use; close the task board pane and run this script again."
    fi
    fail "go build failed (exit $rc): $(detail "$out")$hint"
  fi
  [[ -e "$BUILD_DIR/$EXECUTABLE_NAME" ]] || fail "go build succeeded but $BUILD_DIR/$EXECUTABLE_NAME does not exist."
  echo "      built $BUILD_DIR/$EXECUTABLE_NAME"
}

# The omp: line of `herdr integration status`. Sets INTEGRATION_LINE, which is
# empty when there is none.
read_integration_line() {
  local line
  INTEGRATION_LINE=''
  run "$HERDR" integration status
  (( RUN_CODE == 0 )) || fail "herdr integration status failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
  while IFS= read -r line; do
    line=${line#"${line%%[![:space:]]*}"}
    line=${line%"${line##*[![:space:]]}"}
    if [[ $line == omp:* ]]; then
      INTEGRATION_LINE=$line
      return 0
    fi
  done <<<"$RUN_OUT"
  return 0
}

# The rule internal/setup applies: an omp: line that does not say "not
# installed" counts as installed, outdated included. No omp: line proves nothing.
integration_installed() {
  [[ -n $1 && ${1,,} != *'not installed'* ]]
}

check_integration() {
  echo "[2/4] Checking Herdr's OMP integration"
  read_integration_line
  if ! integration_installed "$INTEGRATION_LINE"; then
    echo '      not installed: running herdr integration install omp'
    run "$HERDR" integration install omp
    (( RUN_CODE == 0 )) || fail "herdr integration install omp failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
    read_integration_line
    integration_installed "$INTEGRATION_LINE" || fail "Herdr still does not report its OMP integration as installed after 'herdr integration install omp' (status line: '$INTEGRATION_LINE')."
  fi
  echo "      $INTEGRATION_LINE"
}

link_plugin() {
  echo "[3/4] Linking build/linux-plugin as a Herdr plugin"
  run "$HERDR" plugin link "$BUILD_DIR"
  (( RUN_CODE == 0 )) || fail "herdr plugin link failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
  get_registration
  [[ -n $REGISTRATION ]] || fail "Herdr does not list $PLUGIN_ID after 'herdr plugin link'."
  assert_no_other_link
  if [[ $(reg_get '.enabled // false') != true ]]; then
    echo '      disabled: running herdr plugin enable'
    run "$HERDR" plugin enable "$PLUGIN_ID"
    (( RUN_CODE == 0 )) || fail "herdr plugin enable failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
    get_registration
    if [[ -z $REGISTRATION || $(reg_get '.enabled // false') != true ]]; then
      fail "Herdr still reports $PLUGIN_ID as disabled after 'herdr plugin enable'."
    fi
  fi
  if (( $(reg_get '(.warnings // []) | length') > 0 )); then
    fail "Herdr reports manifest warnings for $PLUGIN_ID: $(detail "$(reg_json '.warnings // []')"). A warning means part of herdr-plugin.toml did not load."
  fi
  if [[ $(reg_get '[.actions[]? | select(.id == "setup")] | length') == 0 ]]; then
    fail "Herdr lists no setup action for $PLUGIN_ID."
  fi
  echo "      linked $PLUGIN_ID from $(reg_get '.plugin_root // ""') (enabled, no warnings)"
}

# Polls Herdr's plugin log once a second until the action's entry is finished.
# Sets LOG_ENTRY. Herdr records the action's own stdout there, which is the
# answer setup gave.
wait_plugin_log() {
  local log_id=$1 deadline entry
  deadline=$(( SECONDS + SETUP_LOG_TIMEOUT_SECONDS ))
  while :; do
    run "$HERDR" plugin log list --plugin "$PLUGIN_ID" --limit 100
    (( RUN_CODE == 0 )) || fail "herdr plugin log list failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
    require_json "$RUN_OUT" 'herdr plugin log list'
    entry=$(jq -c --arg id "$log_id" '[.result.logs[]? | select(.log_id == $id)][0] // null' <<<"$RUN_OUT")
    if [[ $entry != null && $(jq -r '.status // ""' <<<"$entry") != running ]]; then
      LOG_ENTRY=$entry
      return 0
    fi
    if (( SECONDS > deadline )); then
      fail "the setup action (log $log_id) did not finish within $SETUP_LOG_TIMEOUT_SECONDS seconds; see 'herdr plugin log list --plugin $PLUGIN_ID'."
    fi
    sleep 1
  done
}

run_setup() {
  local log_id stdout_text exit_code stderr_text
  echo "[4/4] Running the plugin's setup action (writes the OMP extension for the active profile)"
  run "$HERDR" plugin action invoke setup --plugin "$PLUGIN_ID"
  (( RUN_CODE == 0 )) || fail "herdr plugin action invoke setup failed (exit $RUN_CODE): $(detail "$RUN_ERR")"
  require_json "$RUN_OUT" 'herdr plugin action invoke'
  log_id=$(jq -r '.result.log.log_id // ""' <<<"$RUN_OUT")
  [[ -n $log_id ]] || fail 'herdr plugin action invoke gave no log id for the setup action.'
  wait_plugin_log "$log_id"
  stdout_text=$(jq -r '.stdout // ""' <<<"$LOG_ENTRY")
  exit_code=$(jq -r '.exit_code // "null"' <<<"$LOG_ENTRY")
  if is_json "$stdout_text" && [[ $exit_code == 0 ]] && [[ $(jq -r '.ok // false' <<<"$stdout_text") == true ]]; then
    echo "      extension: $(jq -r '.extension // ""' <<<"$stdout_text")"
  elif is_json "$stdout_text" && jq -e '.error != null' <<<"$stdout_text" >/dev/null 2>&1; then
    fail "setup refused: $(jq -r '.error.code // ""' <<<"$stdout_text"): $(jq -r '.error.message // ""' <<<"$stdout_text")"
  else
    stderr_text=$(jq -r '.stderr // ""' <<<"$LOG_ENTRY")
    fail "the setup action exited $exit_code without a setup answer. stdout: $(detail "$stdout_text"). stderr: $(detail "$stderr_text")"
  fi
}

main() {
  preflight
  build_plugin
  check_integration
  link_plugin
  run_setup
  echo ''
  echo 'Installed. Use the plugin actions in Herdr:'
  echo '  bind   Bind this repository as an orchestrated project (run from a pane in the repository)'
  echo '  board  Open the task board'
}

main
