#!/usr/bin/env bash
# Branch tests for install.sh, run against stub herdr, go and uname executables.
#
# Each scenario copies install.sh and linux/herdr-plugin.toml into a fresh folder
# under a directory this run owns, puts stub herdr, go and uname first on PATH,
# and runs the copy with a PATH that holds nothing but the commands named in
# install.sh's `# tools:` line, plus jq. So no real go or herdr can be found. The
# stubs answer from a state file and log every call, so each scenario checks the
# exit status, the message the user reads, and which Herdr and go commands ran.
# Nothing here starts Herdr or builds the plugin for real, and nothing reads or
# writes a real Herdr, Go or OMP installation.
#
# Output: one "ok" or "FAIL" line per scenario, then
# "install script check failures: N of M scenarios". Exit 1 on any failure.
#
# Rebuilding over a running binary (plan Task 7, Step 5; a measurement, not a
# test). Measured on this WSL2 host (Linux 6.18, Go 1.27.1, uutils coreutils),
# with `CGO_ENABLED=0 go build -o <path>` on a one-line module
# (`package main; func main() {}`) while a copy of /usr/bin/sleep ran from <path>
# in the background. The build succeeded (exit 0) and the process kept running,
# with the target on /tmp (tmpfs) and with GOTMPDIR on another filesystem. No
# "text file busy" appeared. The copy has to keep the name `sleep`: this coreutils
# build refuses to run a copy under any other name, so the copy ran from
# <tmp>/bin/sleep and was the target of the build. install.sh's task-board hint
# therefore covers a host that reports "text file busy", which this host did not.

set -uo pipefail

TESTS_DIR=$(cd "$(dirname "$0")" && pwd)
PLUGIN_ROOT=$(cd "$TESTS_DIR/.." && pwd)
INSTALL_SH="$PLUGIN_ROOT/install.sh"
MANIFEST="$PLUGIN_ROOT/linux/herdr-plugin.toml"
if [[ ! -f $INSTALL_SH || ! -f $MANIFEST ]]; then
  echo "install.sh and linux/herdr-plugin.toml must sit in $PLUGIN_ROOT" >&2
  exit 1
fi

# The commands install.sh declares in its `# tools:` line. Each scenario links
# exactly these from the system, plus jq.
TOOLS=()
while IFS= read -r line; do
  if [[ $line == '# tools:'* ]]; then
    read -ra TOOLS <<<"${line#'# tools:'}"
    break
  fi
done <"$INSTALL_SH"
if (( ${#TOOLS[@]} == 0 )); then
  echo "install.sh has no '# tools:' line" >&2
  exit 1
fi

RUN_ROOT=$(mktemp -d)
trap 'rm -rf -- "$RUN_ROOT"' EXIT
STUBS="$RUN_ROOT/stubs"
mkdir -p "$STUBS"

# Stub herdr. Logs every call to calls.log and argv.log, then answers from the
# state file. It uses only jq and shell builtins, because a scenario's PATH holds
# nothing else.
cat >"$STUBS/herdr" <<'STUB'
#!/bin/sh
line=herdr
argv=
for a in "$@"; do
  line="$line $a"
  argv="$argv[$a]"
done
printf '%s\n' "$line" >>"$STUB_LOG"
printf '%s\n' "$argv" >>"$STUB_ARGV"

st() { jq -r "$1" "$STUB_STATE"; }
update() {
  new=$(jq "$@" "$STUB_STATE") || exit 98
  printf '%s\n' "$new" >"$STUB_STATE"
}

case "$1 $2" in
'plugin list')
  if [ "$(st .listExit)" != 0 ]; then
    echo 'stub: herdr is not answering' >&2
    exit "$(st .listExit)"
  fi
  jq -c '{id: "cli:plugin", result: {plugins: (if .registered then [{plugin_id: "user.omp-orchestrator", plugin_root: .root, enabled: .enabled, actions: [{id: "setup"}, {id: "bind"}, {id: "board"}]} + (if (.warnings | length) > 0 then {warnings: .warnings} else {} end)] else [] end)}}' "$STUB_STATE"
  ;;
'plugin link')
  if [ "$(st .linkExit)" != 0 ]; then
    st .linkStderr >&2
    exit "$(st .linkExit)"
  fi
  update --arg p "$3" 'if .registered then . else .enabled = true end | .registered = true | .root = $p'
  jq -c -n --arg p "$3" '{id: "cli:plugin", result: {linked: $p}}'
  ;;
'plugin enable')
  if [ "$(st .enableExit)" != 0 ]; then
    echo 'enable failed' >&2
    exit "$(st .enableExit)"
  fi
  update '.enabled = true'
  ;;
'plugin action')
  if [ "$3" != invoke ]; then
    echo "stub: unexpected herdr call: $*" >&2
    exit 99
  fi
  if [ "$(st .invokeExit)" != 0 ]; then
    echo 'invoke failed' >&2
    exit "$(st .invokeExit)"
  fi
  update '.polls = 0'
  jq -c -n '{id: "cli:plugin", result: {log: {log_id: "plugin-log-901"}}}'
  ;;
'plugin log')
  polls=$(( $(st .polls) + 1 ))
  update --argjson n "$polls" '.polls = $n'
  status=succeeded
  if [ "$polls" -le "$(st .runningPolls)" ]; then status=running; fi
  jq -c -n --arg s "$status" --argjson code "$(st .setupExit)" --arg out "$(st .setupStdout)" '{id: "cli:plugin", result: {logs: [{log_id: "plugin-log-901", status: $s, exit_code: $code, stdout: $out, stderr: ""}]}}'
  ;;
'integration status')
  printf '%s\n' 'pi: not installed (/home/test/.pi/agent/extensions/herdr-agent-state.ts)'
  case "$(st .omp)" in
  current) printf '%s\n' 'omp: current (v10) (/home/test/.omp/agent/extensions/herdr-omp-agent-state.ts)' ;;
  outdated) printf '%s\n' 'omp: outdated (v9) (/home/test/.omp/agent/extensions/herdr-omp-agent-state.ts)' ;;
  *) printf '%s\n' 'omp: not installed (/home/test/.omp/agent/extensions/herdr-omp-agent-state.ts)' ;;
  esac
  ;;
'integration install')
  if [ "$(st .ompInstallExit)" != 0 ]; then
    echo 'integration install failed' >&2
    exit "$(st .ompInstallExit)"
  fi
  if [ "$(st .ompStaysMissing)" != true ]; then update '.omp = "current"'; fi
  ;;
*)
  echo "stub: unexpected herdr call: $*" >&2
  exit 99
  ;;
esac
STUB

# Stub go. Logs the call with CGO_ENABLED, fails with GO_EXIT and GO_STDERR when
# GO_EXIT is nonzero, and writes a placeholder to the path after -o unless
# GO_NO_OUTPUT is 1.
cat >"$STUBS/go" <<'STUB'
#!/bin/sh
line="go CGO_ENABLED=[${CGO_ENABLED-}]"
out=
prev=
for a in "$@"; do
  line="$line $a"
  if [ "$prev" = -o ]; then out=$a; fi
  prev=$a
done
printf '%s\n' "$line" >>"$STUB_LOG"
if [ -n "${GO_EXIT-}" ] && [ "$GO_EXIT" != 0 ]; then
  printf '%s\n' "${GO_STDERR-}" >&2
  exit "$GO_EXIT"
fi
if [ "${GO_NO_OUTPUT-}" != 1 ]; then
  printf '%s\n' placeholder >"$out"
fi
exit 0
STUB

# Stub uname. Prints Linux, or whatever UNAME_OS says.
cat >"$STUBS/uname" <<'STUB'
#!/bin/sh
printf '%s\n' "${UNAME_OS:-Linux}"
STUB
chmod 755 "$STUBS/herdr" "$STUBS/go" "$STUBS/uname"

# The state a scenario starts from, before its own overrides. setupStdout is the
# answer the setup action gives on success.
DEFAULT_STATE='{"registered":false,"root":"","enabled":true,"warnings":[],"omp":"missing","ompInstallExit":0,"ompStaysMissing":false,"listExit":0,"linkExit":0,"linkStderr":"","enableExit":0,"invokeExit":0,"runningPolls":0,"setupExit":0,"setupStdout":"{\"ok\":true,\"extension\":\"/home/test/.omp/agent/extensions/herdr-orchestrator.ts\",\"protocol\":1}","polls":0}'

BUILD_CALL='^go CGO_ENABLED=\[0\] build -o .*/build/linux-plugin/herdr-omp-orchestrator \./cmd/herdr-omp-orchestrator$'
INVOKE_CALL='^herdr plugin action invoke setup --plugin user\.omp-orchestrator$'
ENABLE_CALL='^herdr plugin enable user\.omp-orchestrator$'

checks=0
fails=0
CASE_NO=0

# Starts a scenario. Each scenario sets these fields, then calls run_case:
#   c_exit      expected exit status
#   c_folder    the fake repository's folder name
#   c_bin       which of the stubs are on PATH: full, no-herdr or no-go
#   c_omit      1 to leave linux/herdr-plugin.toml out of the fake repository
#   c_symlink   1 to make a symlink to the fake repository at fake-link
#   c_skip_jq   1 to leave jq off PATH
#   c_state     JSON overriding the stub state; @BUILD@, @LINK@ and @ELSEWHERE@
#               name paths
#   c_env       extra environment assignments
#   c_out       patterns the output must match
#   c_calls     patterns at least one Herdr or go call must match
#   c_nocalls   patterns no call may match
#   c_argv      exact argument lines a Herdr call must receive; @FAKE@ is the repo
#   c_min_pat   a call pattern that must match at least c_min times
begin() {
  CASE_NO=$((CASE_NO + 1))
  CASE_DIR="$RUN_ROOT/case-$CASE_NO"
  c_name=$1 c_exit=0 c_folder=fake-repo c_bin=full c_omit=0 c_symlink=0 c_skip_jq=0
  c_min_pat='' c_min=0
  c_env=() c_state='{}' c_out=() c_calls=() c_nocalls=() c_argv=()
}

short() {
  local text=$1
  text=${text//$'\n'/ | }
  if (( ${#text} > 1200 )); then text="${text:0:1200}..."; fi
  printf '%s' "$text"
}

# Links the commands install.sh declares, plus jq, into one scenario's tool folder.
link_tools() {
  local name path
  for name in "${TOOLS[@]}" jq; do
    if [[ $name == jq ]] && (( c_skip_jq )); then continue; fi
    if [[ -e $1/$name ]]; then continue; fi
    path=$(command -v "$name") || { echo "harness: '$name' is not installed on this machine" >&2; exit 1; }
    ln -s "$path" "$1/$name"
  done
}

count_matches() {
  local pat=$1 line n=0
  shift
  for line in "$@"; do
    if [[ $line =~ $pat ]]; then n=$((n + 1)); fi
  done
  printf '%s' "$n"
}

first_match() {
  local pat=$1 line
  shift
  for line in "$@"; do
    if [[ $line =~ $pat ]]; then
      printf '%s' "$line"
      return 0
    fi
  done
}

run_case() {
  local fake bin code output pat want line n hit joined problem found
  local -a calls argvs problems
  fake="$CASE_DIR/$c_folder"
  mkdir -p "$fake/linux" "$CASE_DIR/home" "$CASE_DIR/bin" "$CASE_DIR/tools"
  cp "$INSTALL_SH" "$fake/install.sh"
  if (( ! c_omit )); then cp "$MANIFEST" "$fake/linux/herdr-plugin.toml"; fi
  if (( c_symlink )); then ln -s "$fake" "$CASE_DIR/fake-link"; fi

  c_state=${c_state//@BUILD@/$fake/build/linux-plugin}
  c_state=${c_state//@LINK@/$CASE_DIR/fake-link/build/linux-plugin}
  c_state=${c_state//@ELSEWHERE@/$CASE_DIR/elsewhere/checkout}
  printf '%s\n' "$(jq -c -n --argjson d "$DEFAULT_STATE" --argjson o "$c_state" '$d + $o')" >"$CASE_DIR/state.json"

  bin="$CASE_DIR/bin"
  cp "$STUBS/uname" "$bin/"
  case $c_bin in
    full) cp "$STUBS/herdr" "$STUBS/go" "$bin/" ;;
    no-herdr) cp "$STUBS/go" "$bin/" ;;
    no-go) cp "$STUBS/herdr" "$bin/" ;;
  esac
  link_tools "$CASE_DIR/tools"

  : >"$CASE_DIR/calls.log"
  : >"$CASE_DIR/argv.log"
  env -i PATH="$bin:$CASE_DIR/tools" HOME="$CASE_DIR/home" \
    STUB_STATE="$CASE_DIR/state.json" STUB_LOG="$CASE_DIR/calls.log" STUB_ARGV="$CASE_DIR/argv.log" \
    "${c_env[@]}" "$BASH" "$fake/install.sh" >"$CASE_DIR/output" 2>&1
  code=$?

  output=$(<"$CASE_DIR/output")
  mapfile -t calls <"$CASE_DIR/calls.log"
  mapfile -t argvs <"$CASE_DIR/argv.log"
  problems=()
  (( code == c_exit )) || problems+=("exit $code, want $c_exit")
  for pat in "${c_out[@]}"; do
    [[ $output =~ $pat ]] || problems+=("output lacks /$pat/")
  done
  for pat in "${c_calls[@]}"; do
    (( $(count_matches "$pat" "${calls[@]}") > 0 )) || problems+=("no call matches /$pat/")
  done
  for pat in "${c_nocalls[@]}"; do
    hit=$(first_match "$pat" "${calls[@]}")
    if [[ -n $hit ]]; then problems+=("unexpected call matches /$pat/: $hit"); fi
  done
  for want in "${c_argv[@]}"; do
    want=${want//@FAKE@/$fake}
    found=0
    for line in "${argvs[@]}"; do
      if [[ $line == "$want" ]]; then found=1; fi
    done
    (( found )) || problems+=("no call received exactly $want")
  done
  if [[ -n $c_min_pat ]]; then
    n=$(count_matches "$c_min_pat" "${calls[@]}")
    (( n >= c_min )) || problems+=("matched $n calls to /$c_min_pat/, want at least $c_min")
  fi

  checks=$((checks + 1))
  if (( ${#problems[@]} == 0 )); then
    printf 'ok   %s\n' "$c_name"
    return 0
  fi
  fails=$((fails + 1))
  joined=''
  for problem in "${problems[@]}"; do joined+="${joined:+; }$problem"; done
  printf 'FAIL %s :: %s | output: %s | calls: %s\n' "$c_name" "$joined" \
    "$(short "$output")" "$(short "$(printf '%s\n' "${calls[@]}")")"
}

begin 'fresh install runs every step'
c_out=('Installed\.' 'extension: ')
c_calls=("$BUILD_CALL" '^herdr integration install omp$' '^herdr plugin link ' "$INVOKE_CALL")
c_nocalls=('plugin unlink' 'plugin enable')
run_case

begin 'already linked and installed: no reinstall, no unlink'
c_state='{"registered":true,"root":"@BUILD@","omp":"current"}'
c_out=('Installed\.')
c_calls=('^herdr plugin link ' '^herdr plugin action invoke setup')
c_nocalls=('integration install' 'plugin unlink')
run_case

begin 'a symlink to the same folder is the same folder'
c_symlink=1
c_state='{"registered":true,"root":"@LINK@","omp":"current"}'
c_out=('Installed\.')
c_nocalls=('plugin unlink')
run_case

begin 'an outdated integration counts as installed'
c_state='{"omp":"outdated"}'
c_out=('Installed\.')
c_nocalls=('integration install')
run_case

begin 'a link to another folder stops before any change'
c_state='{"registered":true,"root":"@ELSEWHERE@"}'
c_exit=1
c_out=('herdr plugin unlink user\.omp-orchestrator')
c_nocalls=('^go ' 'integration install' 'plugin link' 'action invoke' 'plugin enable')
run_case

begin 'a disabled plugin is enabled'
c_state='{"registered":true,"root":"@BUILD@","enabled":false,"omp":"current"}'
c_out=('Installed\.')
c_calls=("$ENABLE_CALL")
c_nocalls=('plugin unlink')
run_case

begin 'manifest warnings stop before setup'
c_state='{"warnings":["unknown event pane.frobnicated"]}'
c_exit=1
c_out=('manifest warnings')
c_nocalls=('action invoke')
run_case

begin 'a setup refusal names its code'
c_state='{"omp":"current","setupExit":1,"setupStdout":"{\"ok\":false,\"error\":{\"code\":\"integration_missing\",\"message\":\"Herdr reports its OMP integration is not installed\"}}"}'
c_exit=1
c_out=('setup refused: integration_missing')
run_case

begin 'a setup action still running is awaited'
c_state='{"omp":"current","runningPolls":2}'
c_out=('Installed\.')
c_min_pat='^herdr plugin log list'
c_min=3
run_case

begin 'a build failure stops before Herdr is changed'
c_env=(GO_EXIT=1 'GO_STDERR=boom: cannot compile')
c_exit=1
c_out=('go build failed' 'boom: cannot compile')
c_nocalls=('plugin link' 'integration install' 'action invoke')
run_case

begin 'a build blocked by a running executable names the task board'
c_state='{"omp":"current"}'
c_env=(GO_EXIT=1 'GO_STDERR=open /tmp/herdr/herdr-omp-orchestrator: text file busy')
c_exit=1
c_out=('close the task board pane')
run_case

begin 'herdr missing from PATH stops at once'
c_bin=no-herdr
c_exit=1
c_out=('herdr was not found on PATH')
c_nocalls=('^herdr ' '^go ')
run_case

begin 'go missing from PATH stops at once'
c_bin=no-go
c_exit=1
c_out=('go was not found on PATH')
c_nocalls=('^herdr ' '^go ')
run_case

begin 'a failing herdr plugin list stops before anything is built'
c_state='{"listExit":1}'
c_exit=1
c_out=('herdr plugin list failed')
c_nocalls=('^go ' 'integration install' 'plugin link' 'action invoke')
run_case

begin 'a link failure stops before setup'
c_state='{"omp":"current","linkExit":1,"linkStderr":"plugin requires a newer Herdr"}'
c_exit=1
c_out=('herdr plugin link failed' 'requires a newer Herdr')
c_nocalls=('action invoke')
run_case

begin 'an integration install failure stops before the link'
c_state='{"ompInstallExit":1}'
c_exit=1
c_out=('herdr integration install omp failed')
c_nocalls=('plugin link' 'action invoke')
run_case

begin 'an integration still missing after install stops'
c_state='{"ompStaysMissing":true}'
c_exit=1
c_out=('still does not report')
c_nocalls=('plugin link' 'action invoke')
run_case

begin 'a folder without the manifest stops before Herdr is asked'
c_omit=1
c_exit=1
c_out=('linux/herdr-plugin\.toml is not in')
c_nocalls=('^herdr ' '^go ')
run_case

begin 'a repository path with spaces reaches herdr as one argument'
c_folder='fake repo'
c_state='{"omp":"current"}'
c_out=('Installed\.')
c_argv=('[plugin][link][@FAKE@/build/linux-plugin]')
c_calls=("$INVOKE_CALL")
run_case

begin 'jq missing from PATH stops at once'
c_skip_jq=1
c_exit=1
c_out=('jq was not found on PATH')
c_nocalls=('^herdr ' '^go ')
run_case

begin 'a non-Linux host is refused'
c_env=(UNAME_OS=Darwin)
c_exit=1
c_out=('install\.sh supports Linux only')
c_nocalls=('^herdr ' '^go ')
run_case

begin 'a build that writes no binary is reported'
c_env=(GO_NO_OUTPUT=1)
c_exit=1
c_out=('go build succeeded but')
c_nocalls=('plugin link')
run_case

printf '\n'
printf 'install script check failures: %d of %d scenarios\n' "$fails" "$checks"
if (( fails > 0 )); then exit 1; fi
exit 0
