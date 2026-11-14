#!/bin/sh
# shellcheck disable=SC2016
set -eu

# install-agent.sh 集成测试。每次安装器调用均受假根测试模式保护；
# 本测试不调用 sudo，也不写入宿主路径。
# 桩函数体有意使用单引号，使变量在桩脚本运行时展开，而非在框架写入时展开。

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALLER=$SCRIPT_DIR/install-agent.sh
UNIT_SOURCE=$SCRIPT_DIR/systemd/orbitbeacon-agent.service
WORK_DIR=$(mktemp -d "/tmp/orbitbeacon-install-test.XXXXXX")
PASS_COUNT=0

cleanup() {
	[ "${KEEP_TEST_TMP:-0}" = 0 ] || return 0
	case $WORK_DIR in
		/tmp/orbitbeacon-install-test.*) rm -rf -- "$WORK_DIR" ;;
	esac
}
trap cleanup EXIT HUP INT TERM

# 使用项目已有 YAML 依赖验证真实语义，不依赖 PyYAML 或另写解析器。
YAML_DECODER=$WORK_DIR/read-yaml
export YAML_DECODER
cat >"$WORK_DIR/read-yaml.go" <<'GO'
package main

import (
    "encoding/json"
    "io"
    "os"

    "gopkg.in/yaml.v3"
)

func main() {
    file, err := os.Open(os.Args[1])
    if err != nil { panic(err) }
    defer file.Close()
    decoder := yaml.NewDecoder(file)
    var value map[string]any
    if err := decoder.Decode(&value); err != nil { panic(err) }
    if value == nil { panic("expected a YAML mapping") }
    var extra any
    if err := decoder.Decode(&extra); err != io.EOF { panic("expected one YAML document") }
    if err := json.NewEncoder(os.Stdout).Encode(value); err != nil { panic(err) }
}
GO
(cd "$SCRIPT_DIR/.." && go build -o "$YAML_DECODER" "$WORK_DIR/read-yaml.go")

fail() {
	printf 'not ok - %s\n' "$*" >&2
	exit 1
}

pass() {
	PASS_COUNT=$((PASS_COUNT + 1))
	printf 'ok %s - %s\n' "$PASS_COUNT" "$1"
}

assert_exists() {
	[ -e "$1" ] || fail "expected $1 to exist"
}

assert_not_exists() {
	[ ! -e "$1" ] || fail "expected $1 not to exist"
}

assert_mode() {
	actual=$(stat -c '%a' "$1")
	[ "$actual" = "$2" ] || fail "mode of $1: expected $2, got $actual"
}

assert_log_has() {
	needle=$1
	grep -F -- "$needle" "$TEST_LOG" >/dev/null || fail "log missing: $needle"
}

assert_log_lacks() {
	needle=$1
	if grep -F -- "$needle" "$TEST_LOG" >/dev/null 2>&1; then
		fail "log unexpectedly contains: $needle"
	fi
}

write_stub() {
	name=$1
	shift
	stub=$CASE_DIR/bin/$name
	apply_body=$1
	printf '%s\n' '#!/bin/sh' 'set -eu' "$apply_body" >"$stub"
	chmod 755 "$stub"
}

setup_case() {
	case_name=$1
	CASE_DIR=$WORK_DIR/$case_name
	TEST_ROOT=$CASE_DIR/root
	TEST_LOG=$CASE_DIR/commands.log
	TEST_FIXTURES=$CASE_DIR/fixtures
	mkdir -p "$CASE_DIR/bin" "$TEST_ROOT/tmp" "$TEST_ROOT/run/systemd/system" "$TEST_FIXTURES"
	: >"$TEST_ROOT/.orbitbeacon-install-test-root"
	mkdir -p "$TEST_ROOT/etc"
	printf '%s\n' 'ID=debian' 'VERSION_ID="12"' >"$TEST_ROOT/etc/os-release"
	: >"$TEST_LOG"
	printf '%s\n' 'test CA certificate' >"$TEST_FIXTURES/ca.pem"

	cat >"$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" <<'AGENT'
#!/bin/sh
set -eu
# 只有降权命令桩设置模拟 UID；直接调用安装后二进制会立即失败。
[ "${TEST_CALLER_UID:-0}" = 12345 ] || exit 23
printf 'agent caller_uid=%s host_uid=%s\n' "$TEST_CALLER_UID" "$(/usr/bin/id -u)" >>"$TEST_LOG"
printf 'agent %s\n' "$*" >>"$TEST_LOG"
if [ "${1:-}" = enroll ]; then
	shift
	token_file=
	while [ "$#" -gt 0 ]; do
		case $1 in
			--token-file) token_file=$2; shift 2 ;;
			*) shift ;;
		esac
	done
	[ -n "$token_file" ] || exit 20
	[ -s "$token_file" ] || exit 21
	if [ -n "${TEST_EXPECT_KEY_FILE:-}" ]; then cmp -s "$token_file" "$TEST_EXPECT_KEY_FILE" || exit 22; fi
	# A noisy Agent must not leak the key through installer output or retained logs.
	cat "$token_file"
	cat "$token_file" >&2
	[ "${TEST_ENROLL_FAIL:-0}" != 1 ] || exit 17
	mkdir -p "$TEST_ROOT/opt/orbit-beacon/state"
	printf '%s\n' '{"node_id":"test-node"}' >"$TEST_ROOT/opt/orbit-beacon/state/identity.json"
	printf '%s\n' enrolled >>"$TEST_ROOT/enrollments.log"
fi
AGENT
	cp "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" "$TEST_FIXTURES/orbitbeacon-agent-linux-arm64"
	printf '%s\n' '# arm64 fixture' >>"$TEST_FIXTURES/orbitbeacon-agent-linux-arm64"
	for fixture_arch in amd64 arm64; do
		chmod 755 "$TEST_FIXTURES/orbitbeacon-agent-linux-$fixture_arch"
		printf '%s\n' "server $fixture_arch fixture" >"$TEST_FIXTURES/orbitbeacon-server-linux-$fixture_arch"
	done

	write_stub id '
if [ "${1:-}" = -u ] && [ "$#" -eq 1 ]; then printf "%s\\n" 0; exit 0; fi
if [ "${2:-}" = orbitbeacon-agent ] && [ -f "$TEST_ROOT/.account-created" ]; then
	case $1 in
		-u) if [ "${TEST_ACCOUNT_UID0:-0}" = 1 ]; then printf "%s\\n" 0; else printf "%s\\n" 12345; fi ;;
		-g) printf "%s\\n" 12346 ;;
		-gn) printf "%s\\n" orbitbeacon-agent ;;
		*) exit 1 ;;
	esac
	exit 0
fi
exit 1'
	write_stub uname '
case ${1:-} in
	-s) printf "%s\\n" Linux ;;
	-m) printf "%s\\n" "${TEST_ARCH:-x86_64}" ;;
	*) exit 1 ;;
esac'
	write_stub groupadd 'printf "groupadd %s\\n" "$*" >>"$TEST_LOG"'
	write_stub useradd 'printf "useradd" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"; : >"$TEST_ROOT/.account-created"'
	write_stub adduser 'printf "adduser" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"; : >"$TEST_ROOT/.account-created"'
	write_stub addgroup 'printf "addgroup" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"'
	write_stub getent '
if [ -f "$TEST_ROOT/.account-created" ]; then
	case ${1:-}:${2:-} in
		passwd:orbitbeacon-agent) printf "%s\\n" "orbitbeacon-agent:x:12345:12346::/opt/orbit-beacon/state:/usr/sbin/nologin" ;;
		group:orbitbeacon-agent) printf "%s\\n" "orbitbeacon-agent:x:12346:" ;;
		*) exit 2 ;;
	esac
else
	exit 2
fi'
	# 保存模拟属主，用于非 root 沙箱内校验完整属主表；不对宿主执行 chown。
	write_stub chown '
printf "chown" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"
[ "${1:-}" = -h ] && shift
owner=$1
shift
for owned_path do printf "%s %s\\n" "$owner" "$owned_path" >>"$CASE_DIR/owners.log"; done'
	write_stub mv '
source_path=
destination=
for argument do source_path=$destination; destination=$argument; done
if [ -n "${TEST_SWAP_DESTINATION:-}" ] && [ "$destination" = "$TEST_SWAP_DESTINATION" ]; then
	rm -f -- "$destination"
	ln -s "$TEST_SWAP_TARGET" "$destination"
fi
if [ -f "$CASE_DIR/owners.log" ]; then
	owner=$(awk -v path="$source_path" '\''$2 == path { owner=$1 } END { print owner }'\'' "$CASE_DIR/owners.log")
	[ -z "$owner" ] || printf "%s %s\\n" "$owner" "$destination" >>"$CASE_DIR/owners.log"
fi
exec /bin/mv "$@"'
	write_stub curl '
printf "curl" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"
output=
url=
while [ "$#" -gt 0 ]; do
	case $1 in
		-o|--output) output=$2; shift 2 ;;
		https://*) url=$1; shift ;;
		*) shift ;;
	esac
done
[ -n "$output" ] || exit 2
case $url in
	*/orbitbeacon-agent-linux-amd64|*/orbitbeacon-agent-linux-arm64)
		cp "$TEST_FIXTURES/${url##*/}" "$output"
		;;
	*/SHA256SUMS)
		for checksum_arch in amd64 arm64; do
			for checksum_kind in server agent; do
				asset=orbitbeacon-$checksum_kind-linux-$checksum_arch
				if [ "${TEST_BAD_CHECKSUM_ARCH:-}" = "$checksum_arch" ]; then
					printf "%064d  %s\\n" 0 "$asset"
				else
					hash=$(sha256sum "$TEST_FIXTURES/$asset" | cut -d " " -f 1)
					printf "%s  %s\\n" "$hash" "$asset"
				fi
			done
		done >"$output"
		;;
	*) exit 22 ;;
esac'
	write_stub runuser '
printf "runuser" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"
[ "${1:-}" = -u ] || exit 2
[ "${2:-}" = orbitbeacon-agent ] || exit 2
shift 2
[ "${1:-}" = -- ] || exit 2
shift
TEST_CALLER_UID=12345
export TEST_CALLER_UID
exec "$@"'
	write_stub su '
printf "su" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"
command_string=
[ "${3:-}" = orbitbeacon-agent ] || exit 2
while [ "$#" -gt 0 ]; do
	case $1 in -c) command_string=$2; shift 2 ;; *) shift ;; esac
done
[ -n "$command_string" ] || exit 2
TEST_CALLER_UID=12345
export TEST_CALLER_UID
exec /bin/sh -c "$command_string"'
	write_stub systemctl 'printf "systemctl" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"'

	export CASE_DIR TEST_ROOT TEST_LOG TEST_FIXTURES
	export ORBITBEACON_INSTALL_TEST=1 ORBITBEACON_INSTALL_ROOT="$TEST_ROOT"
	TEST_PATH=$CASE_DIR/bin:/usr/local/bin:/usr/bin:/bin
	export TEST_PATH
}

run_installer() {
	output=$1
	shift
	env PATH="$TEST_PATH" dash "$INSTALLER" "$@" >"$output" 2>&1
}

run_with_token() {
	token=$1
	output=$2
	shift 2
	printf '%s\n' "$token" | env PATH="$TEST_PATH" dash "$INSTALLER" "$@" >"$output" 2>&1
}

[ -f "$INSTALLER" ] || fail "installer not found: $INSTALLER"
[ -f "$UNIT_SOURCE" ] || fail "unit not found: $UNIT_SOURCE"

# --key is only passed to the installer; the Agent still receives a private file.
for key_shell in dash 'busybox ash'; do
    if [ "$key_shell" = 'busybox ash' ] && ! command -v busybox >/dev/null 2>&1; then
        printf '%s\n' 'SKIP - BusyBox ash key enrollment (busybox missing)'
        continue
    fi
    setup_case "key_$(printf '%s' "$key_shell" | tr ' ' _)"
    secret='--key-DO-NOT-LOG'
    printf '%s\n' "$secret" >"$TEST_FIXTURES/expected-key"
    runner=$INSTALLER
    if [ "$key_shell" = 'busybox ash' ]; then
        # Standalone BusyBox applets bypass PATH; functions preserve the fake-root stubs.
        runner=$CASE_DIR/ash-driver
        printf '%s\n' 'set +x' >"$runner"
        for stub in "$CASE_DIR"/bin/*; do
            printf '%s() { "%s" "$@"; }\n' "${stub##*/}" "$stub" >>"$runner"
        done
        printf '. "%s"\n' "$INSTALLER" >>"$runner"
    fi
    # shellcheck disable=SC2086
    printf '%s\n' ignored-stdin | env PATH="$TEST_PATH" TEST_EXPECT_KEY_FILE="$TEST_FIXTURES/expected-key" $key_shell -x "$runner" --server https://monitor.example.com --version 0.1.0 --key "$secret" >"$CASE_DIR/output" 2>&1
    assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
    [ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/enrollment.token")" = "$secret" ] || fail 'Agent received a different key'
    assert_log_has "--token-file $TEST_ROOT/opt/orbit-beacon/state/enrollment.token"
    assert_log_lacks "$secret"
    if grep -F -- "$secret" "$CASE_DIR/output" "$CASE_DIR/owners.log" "$TEST_ROOT/enrollments.log" >/dev/null; then fail 'key leaked to output or logs'; fi
    pass "$key_shell --key enrolls through a file without leaking to output or child argv/logs, even with xtrace"
done

setup_case key_conflict
printf '%s\n' file-key >"$TEST_FIXTURES/key"
for order in key-first file-first; do
    if [ "$order" = key-first ]; then set -- --key ABC --token-file "$TEST_FIXTURES/key"
    else set -- --token-file "$TEST_FIXTURES/key" --key ABC; fi
    if run_installer "$CASE_DIR/output" "$@" </dev/null; then fail 'key/file conflict accepted'; fi
    grep -F -- '--key 与 --token-file' "$CASE_DIR/output" >/dev/null || fail 'missing conflict message'
done
if run_installer "$CASE_DIR/output" --key '' </dev/null; then fail 'empty key accepted'; fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon"
pass '--key rejects an empty value and --token-file in either order before mutation'

setup_case no_key_no_tty
# setsid drops a controlling tty even when the suite was started interactively.
if env PATH="$TEST_PATH" setsid -w dash "$INSTALLER" --server https://monitor.example.com --version 0.1.0 </dev/null >"$CASE_DIR/output" 2>&1; then fail 'empty stdin without tty accepted'; fi
grep -F -- '--key 或终端提示' "$CASE_DIR/output" >/dev/null || fail 'missing key/prompt diagnostic'
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
assert_log_lacks 'systemctl enable'
pass 'empty non-tty stdin without a controlling tty fails with key/prompt guidance'

if command -v script >/dev/null 2>&1; then
    for prompt_stdin in tty empty blank; do
        setup_case "prompt_$prompt_stdin"
        # script provides /dev/tty; delay input until echo is disabled and the prompt is visible.
        cat >"$CASE_DIR/prompt.sh" <<'PROMPT'
#!/bin/sh
set -eu
before=$(stty -g </dev/tty)
set +e
if [ "$PROMPT_STDIN" != tty ]; then
    dash "$INSTALLER" --server https://monitor.example.com --version 0.1.0 </dev/null
else
    dash "$INSTALLER" --server https://monitor.example.com --version 0.1.0
fi
code=$?
set -e
[ "$(stty -g </dev/tty)" = "$before" ]
if [ "$PROMPT_STDIN" = blank ]; then [ "$code" = 1 ]; else [ "$code" = 0 ]; fi
PROMPT
        export INSTALLER
        PROMPT_STDIN=$prompt_stdin python3 - "$CASE_DIR" <<'PY'
import os, pathlib, select, subprocess, sys, time
case = pathlib.Path(sys.argv[1])
proc = subprocess.Popen(['script', '-qec', f'sh "{case}/prompt.sh"', str(case / 'typescript')],
    env={**os.environ, 'PATH': os.environ['TEST_PATH']}, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
output = b''
sent = False
try:
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        ready, _, _ = select.select([proc.stdout], [], [], 0.1)
        if ready:
            chunk = os.read(proc.stdout.fileno(), 4096)
            if not chunk:
                break
            output += chunk
            if not sent and '密钥（隐藏输入）：'.encode() in output:
                proc.stdin.write(b'\n' if os.environ['PROMPT_STDIN'] == 'blank' else b'prompt-secret-DO-NOT-LOG\n'); proc.stdin.flush(); sent = True
    assert sent, output.decode(errors='replace')
    assert proc.wait(timeout=5) == 0, output.decode(errors='replace')
    assert b'prompt-secret-DO-NOT-LOG' not in output, 'prompt echoed the key'
    assert b'prompt-secret-DO-NOT-LOG' not in (case / 'typescript').read_bytes(), 'typescript leaked the key'
finally:
    if proc.poll() is None:
        proc.kill(); proc.wait()
PY
        if [ "$prompt_stdin" = blank ]; then
            assert_not_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
        else
            [ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/enrollment.token")" = prompt-secret-DO-NOT-LOG ] || fail 'prompt key changed'
        fi
        assert_log_lacks prompt-secret-DO-NOT-LOG
        pass "$prompt_stdin stdin uses hidden /dev/tty prompt and restores terminal state"
    done
else
    printf '%s\n' 'SKIP - hidden prompt pseudo-tty tests (script missing)'
fi

setup_case basic
secret='basic-token-DO-NOT-LOG'
run_with_token "$secret" "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --ca-file "$TEST_FIXTURES/ca.pem"
assert_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
assert_exists "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml"
assert_exists "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
assert_mode "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" 640
assert_log_has "chown -h root:12346"
assert_exists "$TEST_ROOT/opt/orbit-beacon/etc/ca.pem"
assert_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
assert_mode "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" 750
assert_mode "$TEST_ROOT/opt/orbit-beacon/etc" 750
assert_mode "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" 640
assert_mode "$TEST_ROOT/opt/orbit-beacon/etc/ca.pem" 640
assert_mode "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service" 644
assert_mode "$TEST_ROOT/opt/orbit-beacon/state" 700
assert_mode "$TEST_ROOT/opt/orbit-beacon" 750
assert_mode "$TEST_ROOT/opt/orbit-beacon/bin" 750
assert_mode "$TEST_ROOT/opt/orbit-beacon/libexec" 755
assert_mode "$TEST_ROOT/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback" 755
cmp -s "$SCRIPT_DIR/orbitbeacon-agent-rollback" "$TEST_ROOT/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback" || fail 'rollback source differs'
grep -Fx 'ExecStartPre=-/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback' "$UNIT_SOURCE" >/dev/null || fail 'rollback unit must run as Agent user'
if grep -F 'ExecStartPre=+' "$UNIT_SOURCE"; then fail 'privileged rollback'; fi
grep -Fx "root:root $TEST_ROOT/opt/orbit-beacon/libexec" "$CASE_DIR/owners.log" >/dev/null || fail 'libexec owner'
grep -E "root:root $TEST_ROOT/opt/orbit-beacon/libexec/.rollback.[A-Za-z0-9]+" "$CASE_DIR/owners.log" >/dev/null || fail 'rollback owner'
for owner_path in \
	"root:12346 $TEST_ROOT/opt/orbit-beacon" \
	"root:12346 $TEST_ROOT/opt/orbit-beacon/etc" \
	"root:12346 $TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" \
	"root:12346 $TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" \
	"root:12346 $TEST_ROOT/opt/orbit-beacon/etc/ca.pem" \
	"12345:12346 $TEST_ROOT/opt/orbit-beacon/bin" \
	"12345:12346 $TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" \
	"12345:12346 $TEST_ROOT/opt/orbit-beacon/state/enrollment.token" \
	"12345:12346 $TEST_ROOT/opt/orbit-beacon/state"; do
	grep -Fx -- "$owner_path" "$CASE_DIR/owners.log" >/dev/null || fail "owner missing: $owner_path"
done
assert_log_has 'agent caller_uid=12345'
cmp -s "$UNIT_SOURCE" "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service" || fail "installed unit differs from repository unit"
python3 - "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$TEST_ROOT" <<'PY'
import json, os, pathlib, subprocess, sys
p = pathlib.Path(sys.argv[1])
root = sys.argv[2]
data = json.loads(subprocess.check_output([os.environ['YAML_DECODER'], str(p)]))
assert data == {
    "server_url": "https://monitor.example.com",
    "update_base_url": "https://github.com/mr-monkeyray/orbit-beacon/releases/download",
    "ca_file": root + "/opt/orbit-beacon/etc/ca.pem",
    "state_dir": root + "/opt/orbit-beacon/state",
    "expected_uid": 12345,
    "expected_gid": 12346,
    "expected_account": "orbitbeacon-agent",
    "install_mode": "native",
}, data
PY
assert_log_has "curl --fail"
assert_log_has "--proto =https"
assert_log_has "--tlsv1.2"
assert_log_has "systemctl daemon-reload"
assert_log_has "systemctl enable orbitbeacon-agent"
assert_log_has "--config $TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
assert_log_lacks "$secret"
if grep -F -- "$secret" "$CASE_DIR/output" >/dev/null 2>&1; then fail "token leaked to installer output"; fi
assert_log_has 'https://github.com/mr-monkeyray/orbit-beacon/releases/download/v0.1.0/'
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["remote_commands"] == ["check_update", "upgrade", "restart", "stop"]'
pass "download install creates valid policy, secure files, synced unit, and does not leak token"

printf '%s\n' '{"node_id":"identity-must-survive"}' >"$TEST_ROOT/opt/orbit-beacon/state/identity.json"
printf '%s\n' 'report_interval_seconds: 5' 'disk_paths: ["/srv/a,b"]' 'probe_targets: []' >"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml"
before_enrollments=$(wc -l <"$TEST_ROOT/enrollments.log")
: >"$TEST_LOG"
run_installer "$CASE_DIR/upgrade-output" --server https://monitor.example.com --version 0.1.0 </dev/null
grep -F 'identity-must-survive' "$TEST_ROOT/opt/orbit-beacon/state/identity.json" >/dev/null || fail "upgrade replaced identity"
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml" || fail "upgrade replaced local runtime settings"
after_enrollments=$(wc -l <"$TEST_ROOT/enrollments.log")
[ "$before_enrollments" = "$after_enrollments" ] || fail "tokenless upgrade enrolled again"
assert_log_has "systemctl restart orbitbeacon-agent"
pass "tokenless re-run preserves identity and restarts upgraded service"

# 验证 Agent 组对 etc 只有读取/遍历权限，bin/state 可写；root 所有权由命令桩记录。
python3 - "$TEST_ROOT/opt/orbit-beacon" <<'PY'
import pathlib, stat, sys
base = pathlib.Path(sys.argv[1])
for path in [base, base / 'etc', *base.joinpath('etc').iterdir()]:
    mode = stat.S_IMODE(path.lstat().st_mode)
    assert not mode & 0o022, (path, oct(mode))
    assert mode & 0o040, (path, oct(mode))
for name in ['bin', 'state']:
    assert base.joinpath(name).lstat().st_mode & 0o200
PY
pass "root-owned configuration grants the Agent group read access without write access"

# 静态检查覆盖未走到的分支，防止新增 version/doctor 等 root 直接调用。
python3 - "$INSTALLER" "$UNIT_SOURCE" <<'PY'
import pathlib, re, sys
script = pathlib.Path(sys.argv[1]).read_text()
calls = [line.strip() for line in script.splitlines() if re.search(r'\$binary["\x27]?\s+(enroll|run|doctor|version)\b', line) and not line.lstrip().startswith('#')]
assert calls, 'no Agent invocation checked'
assert all(line.startswith(('if runuser -u "$account" --', 'if su -s /bin/sh "$account" -c', 'say ')) for line in calls), calls
unit = pathlib.Path(sys.argv[2]).read_text()
assert 'User=orbitbeacon-agent\nGroup=orbitbeacon-agent\n' in unit
assert 'StateDirectory' not in unit
assert 'ProtectSystem=strict\nReadWritePaths=/opt/orbit-beacon/bin /opt/orbit-beacon/state\n' in unit
PY
pass "all installer binary invocations and the service explicitly select the non-root account"

for link_kind in file directory; do
	setup_case "binary_link_$link_kind"
	run_with_token 'first-token' "$CASE_DIR/first-output" --server https://monitor.example.com --version 0.1.0
	if [ "$link_kind" = file ]; then
		printf '%s\n' untouched >"$CASE_DIR/victim"
	else
		mkdir "$CASE_DIR/victim"
		printf '%s\n' untouched >"$CASE_DIR/victim/orbitbeacon-agent"
	fi
	chmod 640 "$CASE_DIR/victim"
	[ "$link_kind" = file ] || chmod 750 "$CASE_DIR/victim"
	before=$(stat -c '%u:%g:%a' "$CASE_DIR/victim")
	rm "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
	ln -s "$CASE_DIR/victim" "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
	# mv 桩在执行前再次植入链接，覆盖预检之后的竞争窗口。
	TEST_SWAP_DESTINATION=$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent TEST_SWAP_TARGET=$CASE_DIR/victim \
		run_installer "$CASE_DIR/reinstall-output" --server https://monitor.example.com --version 0.1.0 </dev/null
	[ ! -L "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" ] || fail 'binary link survived'
	cmp -s "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" || fail 'binary replacement failed'
	[ "$before" = "$(stat -c '%u:%g:%a' "$CASE_DIR/victim")" ] || fail 'binary link target metadata changed'
	if [ "$link_kind" = file ]; then victim_file=$CASE_DIR/victim; else victim_file=$CASE_DIR/victim/orbitbeacon-agent; fi
	[ "$(cat "$victim_file")" = untouched ] || fail 'binary link target was overwritten'
	assert_log_lacks "chown -h 12345:12346 $TEST_ROOT/opt/orbit-beacon/bin/"
	pass "reinstallation replaces a raced binary $link_kind symlink without touching its target"
done

setup_case token_link
run_with_token 'first-token' "$CASE_DIR/first-output" --server https://monitor.example.com --version 0.1.0
printf '%s\n' untouched >"$CASE_DIR/victim"
chmod 640 "$CASE_DIR/victim"
before=$(stat -c '%u:%g:%a' "$CASE_DIR/victim")
TEST_SWAP_DESTINATION=$TEST_ROOT/opt/orbit-beacon/state/enrollment.token TEST_SWAP_TARGET=$CASE_DIR/victim \
	run_with_token 'replacement-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
[ "$(cat "$CASE_DIR/victim")" = untouched ] || fail 'token link target was overwritten'
[ "$before" = "$(stat -c '%u:%g:%a' "$CASE_DIR/victim")" ] || fail 'token link target metadata changed'
assert_mode "$TEST_ROOT/opt/orbit-beacon/state/enrollment.token" 600
pass "enrollment atomically replaces a raced token symlink without root touching its target"

for directory in bin state; do
	setup_case "directory_link_$directory"
	mkdir -p "$TEST_ROOT/opt/orbit-beacon" "$CASE_DIR/victim"
	printf '%s\n' untouched >"$CASE_DIR/victim/file"
	ln -s "$CASE_DIR/victim" "$TEST_ROOT/opt/orbit-beacon/$directory"
	if run_with_token 'directory-link-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
		fail "installer accepted symlinked $directory directory"
	fi
	if run_installer "$CASE_DIR/uninstall-output" --uninstall --yes; then
		fail "uninstaller accepted symlinked $directory directory"
	fi
	[ "$(cat "$CASE_DIR/victim/file")" = untouched ] || fail 'directory symlink target changed'
	assert_log_lacks 'chown'
	pass "install and uninstall reject a symlinked $directory directory before any ownership change"
done

for machine in x86_64 amd64 aarch64 arm64; do
	setup_case "architecture_$machine"
	case $machine in
		x86_64|amd64) expected_arch=amd64; other_arch=arm64 ;;
		*) expected_arch=arm64; other_arch=amd64 ;;
	esac
	TEST_ARCH=$machine TEST_BAD_CHECKSUM_ARCH=$other_arch run_with_token 'architecture-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
	cmp -s "$TEST_FIXTURES/orbitbeacon-agent-linux-$expected_arch" "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" || fail "$machine installed the wrong architecture"
	assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
	pass "$machine installs its architecture-only binary and ignores unrelated checksums"
done

setup_case config_symlink
mkdir -p "$TEST_ROOT/opt/orbit-beacon/etc"
printf '%s\n' '{"report_interval_seconds":5}' >"$CASE_DIR/external.json"
ln -s "$CASE_DIR/external.json" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
if run_with_token 'symlink-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "installer accepted a symlinked runtime configuration"
fi
[ "$(cat "$CASE_DIR/external.json")" = '{"report_interval_seconds":5}' ] || fail "installer modified symlink target"
pass "runtime configuration symlink is rejected without changing its target"

setup_case config_directory
mkdir -p "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
if run_with_token 'directory-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "installer accepted a directory as runtime configuration"
fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
pass "non-regular runtime configuration is rejected before installation"

setup_case foreground
rm -rf -- "$TEST_ROOT/run/systemd"
run_with_token 'foreground-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
grep -F -- "run --install-policy $TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml --config $TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/output" >/dev/null || fail "foreground instructions omitted local configuration"
assert_log_lacks "systemctl enable"
pass "non-systemd instructions use the installed local configuration"

setup_case su_fallback
rm "$CASE_DIR/bin/runuser"
# 限定 PATH，避免找到宿主 runuser；其余命令仍使用普通工具或现有桩。
for tool in dash awk stat mkdir mktemp chmod cp install cat sha256sum stty rm cut sed sync; do
	ln -s "$(command -v "$tool")" "$CASE_DIR/bin/$tool"
done
TEST_PATH=$CASE_DIR/bin
run_with_token 'su-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
assert_log_has 'su -s /bin/sh orbitbeacon-agent -c'
assert_log_has 'agent caller_uid=12345'
assert_log_lacks 'runuser'
pass "su fallback also executes the binary only through the dedicated non-root account"

for machine in x86_64 aarch64 arm64; do
	setup_case "checksum_$machine"
	case $machine in
		x86_64) expected_arch=amd64 ;;
		*) expected_arch=arm64 ;;
	esac
	if TEST_ARCH=$machine TEST_BAD_CHECKSUM_ARCH=$expected_arch run_with_token 'checksum-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
		fail "$machine bad checksum unexpectedly succeeded"
	fi
	assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
	assert_log_lacks "runuser"
	pass "$machine checksum failure stops before binary installation or enrollment"
done

setup_case enroll_failure
failure_secret='failure-token-DO-NOT-LOG'
if TEST_ENROLL_FAIL=1 run_with_token "$failure_secret" "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "enrollment failure unexpectedly succeeded"
fi
token_path=$TEST_ROOT/opt/orbit-beacon/state/enrollment.token
assert_exists "$token_path"
assert_mode "$token_path" 600
assert_mode "$TEST_ROOT/opt/orbit-beacon/state" 700
[ "$(sed -n '1p' "$token_path")" = "$failure_secret" ] || fail "retained token content changed"
assert_log_lacks "$failure_secret"
if grep -F -- "$failure_secret" "$CASE_DIR/output" >/dev/null 2>&1; then fail "failure token leaked to output"; fi
pass "failed enrollment retains a mode-0600 token without leaking it"

setup_case local_binary
local_secret='local-token-DO-NOT-LOG'
printf '%s\n' "$local_secret" >"$TEST_FIXTURES/enrollment.token"
chmod 600 "$TEST_FIXTURES/enrollment.token"
run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --token-file "$TEST_FIXTURES/enrollment.token"
assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
assert_log_lacks "curl"
assert_log_lacks "$local_secret"
pass "local binary install skips release downloads"

http_case=0
for http_server in 'http://monitor.example.com/' 'http://192.0.2.10:8080' 'http://[2001:db8::1]:8080'; do
	http_case=$((http_case + 1))
	setup_case "http_url_$http_case"
	run_with_token 'http-token' "$CASE_DIR/output" --server "$http_server" --version 0.1.0
	python3 - "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "${http_server%/}" <<'PY'
import json, os, subprocess, sys
data = json.loads(subprocess.check_output([os.environ['YAML_DECODER'], sys.argv[1]]))
assert data["server_url"] == sys.argv[2], data
assert data["ca_file"] == "", data
PY
	assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
	assert_log_has "--proto =https"
	assert_log_has "--proto-redir =https"
	assert_log_has "https://github.com/mr-monkeyray/orbit-beacon/releases/download/v0.1.0/"
done
pass "ordinary HTTP server origins are accepted without weakening release download TLS"

setup_case http_ca_conflict
if run_with_token 'http-ca-token' "$CASE_DIR/output" --server http://monitor.example.com --version 0.1.0 --ca-file "$TEST_FIXTURES/missing.pem"; then
	fail "HTTP server accepted a CA file"
fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
assert_log_lacks "curl"
pass "HTTP and CA conflict is rejected before reading CA or installing"

setup_case bad_url
for bad_server in \
	'https://user@monitor.example.com/path?x=1' \
	'https://monitor.example.com"' \
	'https://a..b' \
	'https://[:::]' \
	'http://user@monitor.example.com' \
	'http://monitor.example.com/prefix' \
	'http://monitor.example.com/?x=1' \
	'http://monitor.example.com/#fragment' \
	'http://monitor.example.com:0' \
	'http://monitor.example.com:65536' \
	'ftp://monitor.example.com' \
	'//monitor.example.com'; do
	if run_installer "$CASE_DIR/output" --server "$bad_server" --version 0.1.0 </dev/null; then
		fail "invalid server origin unexpectedly succeeded: $bad_server"
	fi
done
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
pass "server URLs with userinfo, malformed host, paths, queries, or JSON-unsafe characters are rejected"

setup_case ipv6_url
run_with_token 'ipv6-token' "$CASE_DIR/output" --server 'https://[2001:db8::1]:8443' --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
python3 - "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" <<'PY'
import json, os, subprocess, sys
data = json.loads(subprocess.check_output([os.environ['YAML_DECODER'], sys.argv[1]]))
assert data["server_url"] == "https://[2001:db8::1]:8443", data
PY
pass "valid bracketed IPv6 HTTPS origin is accepted and YAML encoded"

setup_case uid_zero
: >"$TEST_ROOT/.account-created"
if TEST_ACCOUNT_UID0=1 run_with_token 'uid-zero-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "uid-zero account unexpectedly accepted"
fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
pass "uid-zero agent account is rejected"

setup_case uninstall
run_with_token 'uninstall-token' "$CASE_DIR/install-output" --server https://monitor.example.com --version 0.1.0 --ca-file "$TEST_FIXTURES/ca.pem"
printf 'n\n' | env PATH="$TEST_PATH" dash "$INSTALLER" --uninstall >"$CASE_DIR/refuse-output" 2>&1 || true
assert_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
assert_exists "$TEST_ROOT/opt/orbit-beacon/state/identity.json"
# 卸载时两个可写目录均可含任意链接；保留外部目标及其属主/模式。
mkdir "$CASE_DIR/victim-dir"
printf '%s\n' untouched >"$CASE_DIR/victim-dir/file"
before=$(stat -c '%u:%g:%a' "$CASE_DIR/victim-dir/file")
ln -s "$CASE_DIR/victim-dir" "$TEST_ROOT/opt/orbit-beacon/bin/directory-link"
ln -s "$CASE_DIR/victim-dir/file" "$TEST_ROOT/opt/orbit-beacon/state/file-link"
: >"$TEST_LOG"
run_installer "$CASE_DIR/uninstall-output" --uninstall --yes
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/libexec"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/etc/ca.pem"
assert_not_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/state"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon"
[ "$(cat "$CASE_DIR/victim-dir/file")" = untouched ] || fail 'uninstall followed an external link'
[ "$before" = "$(stat -c '%u:%g:%a' "$CASE_DIR/victim-dir/file")" ] || fail 'uninstall changed external metadata'
assert_log_lacks 'agent '
assert_log_lacks 'runuser'
assert_log_lacks 'su -s'
assert_log_has "systemctl disable --now orbitbeacon-agent"
assert_log_has "systemctl daemon-reload"
pass "uninstall refuses declined confirmation and removes the layout without following links or executing Agent"

# 回滚只读写固定目录，恶意二进制不得执行。
for scenario in below above missing garbage newline; do
    rollback_root=$WORK_DIR/rollback_$scenario
    mkdir -p "$rollback_root/bin" "$rollback_root/state"
    printf '%s\n' '#!/bin/sh' "touch '$rollback_root/executed'" > "$rollback_root/bin/orbitbeacon-agent"
    chmod 0750 "$rollback_root/bin/orbitbeacon-agent"
    cp "$rollback_root/bin/orbitbeacon-agent" "$rollback_root/original"
    printf '%s' previous > "$rollback_root/bin/orbitbeacon-agent.prev"
    count=2
    case $scenario in above|missing) count=3 ;; garbage) count=invalid ;; esac
    [ "$scenario" != missing ] || rm "$rollback_root/bin/orbitbeacon-agent.prev"
    printf '%s' "$count" > "$rollback_root/state/upgrade-pending"
    [ "$scenario" != newline ] || printf '1\n' > "$rollback_root/state/upgrade-pending"
    sh "$SCRIPT_DIR/orbitbeacon-agent-rollback" "$rollback_root"
    assert_not_exists "$rollback_root/executed"
    case $scenario in
        below|missing|newline) cmp -s "$rollback_root/original" "$rollback_root/bin/orbitbeacon-agent" || fail 'rollback swapped too early'
            assert_exists "$rollback_root/state/upgrade-pending" ;;
        *) [ "$(cat "$rollback_root/bin/orbitbeacon-agent")" = previous ] || fail 'rollback did not restore'
            assert_exists "$rollback_root/state/upgrade-rolled-back"
            assert_not_exists "$rollback_root/state/upgrade-pending" ;;
    esac
    if [ "$scenario" = missing ]; then
        sh "$SCRIPT_DIR/orbitbeacon-agent-rollback" "$rollback_root"
        [ "$(cat "$rollback_root/state/upgrade-pending")" = 4 ] || fail 'rollback counter must saturate'
    fi
    pass "rollback $scenario preserves startup and executes nothing in bin"
done
setup_case update_base
for invalid_base in '' http://release.example.com https://user@release.example.com https://release.example.com/?q=x https://release.example.com/#x https://a..b https://release.example.com:0 https://release.example.com:65536 'https://[:::]' https://release.example.com/bad%escape; do
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url "$invalid_base" </dev/null; then fail 'invalid upgrade source accepted'; fi
done
if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-channel github </dev/null; then fail 'removed channel flag accepted'; fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon"
run_with_token update-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://release.example.com/download/
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); assert "update_channel" not in p and p["update_base_url"] == "https://release.example.com/download"'
assert_log_has 'https://release.example.com/download/v0.1.0/'
cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml"
run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == "https://release.example.com/download"'
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml" || fail 'source reinstall changed runtime'
pass 'source-only flags reject empty/insecure values and preserve custom sources on reinstall'

# Only these generated legacy shapes are eligible for automatic migration.
write_legacy_policy() {
    python3 - "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$1" "$2" <<'EDIT'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
lines = [line for line in path.read_text().splitlines() if not line.startswith(('update_channel:', 'update_base_url:'))]
if sys.argv[2] != 'absent':
    lines.append('update_channel: ' + json.dumps(sys.argv[2]))
if sys.argv[3] != 'omitted':
    lines.append('update_base_url: ' + json.dumps(sys.argv[3]))
path.write_text('\n'.join(lines) + '\n')
EDIT
}

for legacy_case in custom github github-empty official eight-official omitted eight-custom eight-empty disabled disabled-custom; do
    setup_case "source_$legacy_case"
    run_with_token migration-token "$CASE_DIR/install-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --no-start
    channel=absent source=https://github.com/mr-monkeyray/orbit-beacon/releases/download disabled=0
    case $legacy_case in
        official) channel=github ;;
        custom) channel=custom; source=https://releases.example.com/custom ;;
        github) channel=github; source=https://github.com/Other/Repository/releases/download/// ;;
        github-empty) channel=github; source= ;;
        omitted) source=omitted ;;
        eight-custom) source=https://releases.example.com/custom ;;
        eight-empty) source=; disabled=1 ;;
        disabled) channel=disabled; source=; disabled=1 ;;
        disabled-custom) channel=disabled; source=https://releases.example.com/custom; disabled=1 ;;
    esac
    write_legacy_policy "$channel" "$source"
    printf '%s\n' 'report_interval_seconds: 5' 'disk_paths: ["/srv/a,b"]' 'remote_commands: ["check_update", "restart", "upgrade", "stop"]' >"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
    printf '%s\n' '{"node_id":"migration-identity"}' >"$TEST_ROOT/opt/orbit-beacon/state/identity.json"
    cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml"
    : >"$TEST_LOG"
    run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --no-start </dev/null
    "$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); source=sys.argv[1].rstrip("/"); source="https://github.com/mr-monkeyray/orbit-beacon/releases/download" if source in ("", "omitted") or sys.argv[2] == "disabled" else source; assert "update_channel" not in p and p["update_base_url"] == source' "$source" "$channel"
    "$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); assert p["remote_commands"] == (["restart", "stop"] if sys.argv[1] == "1" else ["check_update", "restart", "upgrade", "stop"]); assert p["report_interval_seconds"] == 5 and p["disk_paths"] == ["/srv/a,b"]' "$disabled"
    [ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/identity.json")" = '{"node_id":"migration-identity"}' ] || fail 'migration replaced identity'
    assert_mode "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" 640
    assert_log_lacks curl
    assert_log_lacks 'systemctl restart'
    assert_log_lacks 'systemctl enable'
    if [ "$disabled" = 0 ]; then cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml" || fail 'enabled migration changed runtime'; fi
    cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/migrated-runtime.yaml"
    run_installer "$CASE_DIR/reinstall-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --no-start </dev/null
    cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/migrated-runtime.yaml" || fail 'reinstall changed migrated permissions'
    pass "legacy $legacy_case migrates source and permissions without losing local state"
done

for command_form in block empty missing absent; do
    setup_case "commands_$command_form"
    run_with_token migration-token "$CASE_DIR/install-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
    write_legacy_policy disabled ''
    printf '%s\n' '# preserve settings' 'report_interval_seconds: 5' 'disk_paths: ["/srv/data"]' >"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
    case $command_form in
        block) printf '%s\n' 'remote_commands:' '  - check_update' '  - restart' '  - "upgrade"' "  - 'stop'" >>"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" ;;
        empty) printf '%s\n' 'remote_commands: []' >>"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" ;;
        absent) rm "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" ;;
    esac
    run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" </dev/null
    "$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); assert p["remote_commands"] == ([] if sys.argv[1] == "empty" else ["restart", "stop"]); assert sys.argv[1] == "absent" or p["report_interval_seconds"] == 5' "$command_form"
    pass "disabled migration safely handles $command_form remote permissions"
done

setup_case durable_permissions
run_with_token migration-token "$CASE_DIR/install-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
write_legacy_policy disabled ''
cp "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml"
cp "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before"
write_stub sync 'exit 1'
if run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --update-base-url https://override.example.com/download </dev/null; then fail 'failed permission sync ignored'; fi
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml" || fail 'policy enabled before permissions were durable'
cmp -s "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before" || fail 'binary replaced before permissions were durable'
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["remote_commands"] == ["restart", "stop"]'
write_stub sync 'exit 0'
run_installer "$CASE_DIR/retry-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --update-base-url https://override.example.com/download </dev/null
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["remote_commands"] == ["restart", "stop"]'
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); assert "update_channel" not in p and p["update_base_url"] == "https://override.example.com/download"'
pass 'permissions are durable before disabled policy removal, including source override and retry'

for cutover_failure in policy permissions-sync policy-sync binary; do
    setup_case "cutover_$cutover_failure"
    run_with_token cutover-token "$CASE_DIR/install-output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
    write_legacy_policy disabled ''
    cp "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before"
    printf '%s\n' '# replacement candidate' >>"$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
    if [ "$cutover_failure" = policy ] || [ "$cutover_failure" = binary ]; then
        TEST_FAIL_CUTOVER=$cutover_failure
        export TEST_FAIL_CUTOVER
        write_stub mv '
destination=
for argument do destination=$argument; done
if [ "$TEST_FAIL_CUTOVER" = policy ] && [ "$destination" = "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" ]; then exit 1; fi
if [ "$TEST_FAIL_CUTOVER" = binary ] && [ "$destination" = "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" ]; then exit 1; fi
exec /bin/mv "$@"'
    else
        TEST_FAIL_SYNC=$cutover_failure
        export TEST_FAIL_SYNC
        write_stub sync '
policy=$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml
if [ "$TEST_FAIL_SYNC" = permissions-sync ] || ! grep -q "^update_channel:" "$policy"; then exit 1; fi
exit 0'
    fi
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" </dev/null; then fail "cutover fault ignored: $cutover_failure"; fi
    cmp -s "$CASE_DIR/binary-before" "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" || fail "new binary committed before a durable policy: $cutover_failure"
    "$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["remote_commands"] == ["restart", "stop"]'
    if [ "$cutover_failure" = policy ] || [ "$cutover_failure" = permissions-sync ]; then
        grep -q '^update_channel:' "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" || fail 'old policy lost before cutover'
    else
        "$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; p=json.load(sys.stdin); assert "update_channel" not in p and p["update_base_url"] == "https://github.com/mr-monkeyray/orbit-beacon/releases/download"'
    fi
    unset TEST_FAIL_CUTOVER TEST_FAIL_SYNC
    pass "failed $cutover_failure cutover retains the old binary and disabled update permissions"
done

setup_case source_syntax
run_with_token syntax-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
reinstall_policy=$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml
for unsupported in duplicate complex unknown legacy-unknown; do
    cp "$reinstall_policy" "$CASE_DIR/policy-before.yaml"
    case $unsupported in
        duplicate) printf '%s\n' 'update_base_url: ""' >>"$reinstall_policy" ;;
        complex) printf '%s\n' 'update_base_url: >-' '  https://release.example.com/download' >"$reinstall_policy" ;;
        unknown) printf '%s\n' 'other: "value"' >>"$reinstall_policy" ;;
        legacy-unknown) write_legacy_policy unknown '' ;;
    esac
    cp "$reinstall_policy" "$CASE_DIR/policy-edited.yaml"
    cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml"
    cp "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before"
    : >"$TEST_LOG"
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://override.example.com/download </dev/null; then fail "ambiguous policy accepted: $unsupported"; fi
    cmp -s "$reinstall_policy" "$CASE_DIR/policy-edited.yaml" || fail 'failed migration modified policy'
    cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml" || fail 'failed migration modified runtime'
    cmp -s "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before" || fail 'failed migration modified binary'
    assert_log_lacks curl
    assert_log_lacks chown
    cp "$CASE_DIR/policy-before.yaml" "$reinstall_policy"
done
pass 'unsupported policies fail closed even with an explicit source override'

for unsupported in alias duplicate folded unknown json null; do
    setup_case "runtime_$unsupported"
    run_with_token syntax-token "$CASE_DIR/install-output" --server https://monitor.example.com --version 0.1.0
    write_legacy_policy disabled ''
    case $unsupported in
        alias) printf '%s\n' 'remote_commands: *permissions' ;;
        duplicate) printf '%s\n' 'remote_commands: []' 'remote_commands: ["upgrade"]' ;;
        folded) printf '%s\n' 'remote_commands: >-' '  ["upgrade"]' ;;
        unknown) printf '%s\n' 'unknown: true' ;;
        json) printf '%s\n' '{"remote_commands":["upgrade"]}' ;;
        null) printf '%s\n' 'remote_commands:' ;;
    esac >"$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
    cp "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml"
    cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml"
    cp "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before"
    : >"$TEST_LOG"
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null; then fail "unsupported runtime accepted: $unsupported"; fi
    cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml" || fail 'failed migration stripped disabling policy'
    cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" "$CASE_DIR/runtime-before.yaml" || fail 'failed migration changed runtime'
    cmp -s "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent" "$CASE_DIR/binary-before" || fail 'failed migration changed binary'
    assert_log_lacks curl
    assert_log_lacks chown
done
pass 'unsupported runtime migration fails before replacing persistent files'

for legacy_name in agent install-policy; do
    setup_case "legacy_$legacy_name"
    mkdir -p "$TEST_ROOT/opt/orbit-beacon/etc" "$TEST_ROOT/opt/orbit-beacon/state" "$TEST_ROOT/opt/orbit-beacon/bin"
    printf '%s\n' '{"report_interval_seconds":5}' >"$TEST_ROOT/opt/orbit-beacon/etc/$legacy_name.json"
    printf '%s\n' '{"node_id":"legacy-identity"}' >"$TEST_ROOT/opt/orbit-beacon/state/identity.json"
    printf '%s\n' previous-binary >"$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null; then fail "legacy $legacy_name silently replaced"; fi
    assert_not_exists "$TEST_ROOT/opt/orbit-beacon/etc/$legacy_name.yaml"
    [ "$(cat "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent")" = previous-binary ] || fail 'legacy binary changed'
    [ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/identity.json")" = '{"node_id":"legacy-identity"}' ] || fail 'legacy identity changed'
    [ "$(cat "$TEST_ROOT/opt/orbit-beacon/etc/$legacy_name.json")" = '{"report_interval_seconds":5}' ] || fail 'legacy configuration changed'
    assert_log_lacks 'curl'
    assert_log_lacks 'chown'
    assert_log_lacks 'systemctl'
    grep -F "$TEST_ROOT/opt/orbit-beacon/etc/$legacy_name.json" "$CASE_DIR/output" >/dev/null || fail 'migration source omitted'
    grep -F "$TEST_ROOT/opt/orbit-beacon/etc/$legacy_name.yaml" "$CASE_DIR/output" >/dev/null || fail 'migration destination omitted'
    pass "legacy $legacy_name aborts before mutation and preserves identity"
    run_installer "$CASE_DIR/uninstall-output" --uninstall --yes
    assert_not_exists "$TEST_ROOT/opt/orbit-beacon"
    pass "legacy $legacy_name remains uninstallable"
done

setup_case copied_json
run_with_token copied-token "$CASE_DIR/install-output" --server https://monitor.example.com --version 0.1.0
printf '%s\n' '{"node_id":"copied-identity"}' >"$TEST_ROOT/opt/orbit-beacon/state/identity.json"
printf '%s\n' '{"report_interval_seconds":5,"disk_paths":["/srv/legacy"]}' >"$TEST_ROOT/opt/orbit-beacon/etc/agent.json"
cp "$TEST_ROOT/opt/orbit-beacon/etc/agent.json" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.json" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" || fail 'reinstall replaced copied legacy runtime settings'
[ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/identity.json")" = '{"node_id":"copied-identity"}' ] || fail 'reinstall replaced migrated identity'
pass 'copied JSON runtime content remains usable as YAML without losing identity or settings'
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" >"$TEST_ROOT/opt/orbit-beacon/etc/install-policy.json"
cp "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.json" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml"
cp "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml"
if run_installer "$CASE_DIR/policy-output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://legacy.example.com/download </dev/null; then fail 'manual JSON policy automatically replaced'; fi
cmp -s "$CASE_DIR/policy-before.yaml" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" || fail 'unsupported JSON policy changed'
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.json" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" || fail 'policy migration replaced runtime settings'
pass 'manual JSON policy requires explicit conversion before installer migration'

setup_case policy_symlink
mkdir -p "$TEST_ROOT/opt/orbit-beacon/etc"
printf '%s\n' untouched >"$CASE_DIR/policy-target"
ln -s "$CASE_DIR/policy-target" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml"
if run_with_token policy-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then fail 'symlinked YAML policy accepted'; fi
[ "$(cat "$CASE_DIR/policy-target")" = untouched ] || fail 'symlinked policy target modified'
assert_log_lacks 'chown'
assert_log_lacks 'curl'
pass 'YAML policy symlink is rejected before mutation'

for distro in centos debian ubuntu alpine; do
	setup_case "distro_$distro"
	printf 'ID=%s\nVERSION_ID="test"\n' "$distro" >"$TEST_ROOT/etc/os-release"
	run_with_token os-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --service none --no-start
	assert_not_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
	assert_log_lacks 'systemctl'
	grep -F "$distro" "$CASE_DIR/output" >/dev/null || fail 'distribution not detected'
	"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["install_mode"] == "foreground"'
done
pass 'known distributions support explicit foreground installation without a unit'

setup_case no_start
run_with_token no-start-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --no-start --download-timeout 42 --download-retries 2
assert_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_log_lacks 'systemctl enable'
assert_log_lacks 'systemctl restart'
assert_log_has '--max-time 42'
assert_log_has '--retry 2'
pass 'no-start installs without enabling or restarting and honors download limits'
cp "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" "$CASE_DIR/policy-before.yaml"
if run_installer "$CASE_DIR/conflict-output" --server https://monitor.example.com --version 0.1.0 --service none </dev/null; then fail 'service backend switch left an old unit behind'; fi
cmp -s "$CASE_DIR/policy-before.yaml" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" || fail 'backend conflict changed policy'
assert_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
pass 'service backend conflicts are rejected before changing an installed policy'

setup_case invalid_options
for options in '--service invalid' '--uid 0' '--gid -1' '--download-timeout 0' '--download-retries -1' '--version --no-start'; do
	# 参数均为测试常量，有意按单词拆分。
	# shellcheck disable=SC2086
	if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 $options </dev/null; then fail "accepted $options"; fi
	assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
done
for machine in i686 armv7l; do
	if TEST_ARCH=$machine run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null; then fail "accepted $machine"; fi
done
pass 'unsupported options and 32-bit machines fail before installing'

setup_case account_mismatch
: >"$TEST_ROOT/.account-created"
if run_with_token account-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --uid 1000; then fail 'existing UID mismatch accepted'; fi
assert_not_exists "$TEST_ROOT/opt/orbit-beacon/bin/orbitbeacon-agent"
pass 'explicit UID cannot silently change an existing account'

setup_case openrc
rm -rf "$TEST_ROOT/run/systemd"
write_stub rc-service 'printf "rc-service %s\\n" "$*" >>"$TEST_LOG"'
write_stub rc-update 'printf "rc-update %s\\n" "$*" >>"$TEST_LOG"'
write_stub start-stop-daemon 'exit 0'
run_with_token openrc-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --no-start
assert_exists "$TEST_ROOT/etc/init.d/orbitbeacon-agent"
assert_mode "$TEST_ROOT/etc/init.d/orbitbeacon-agent" 755
assert_exists "$TEST_ROOT/opt/orbit-beacon/libexec/orbitbeacon-agent-supervise"
assert_not_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_log_lacks 'rc-service'
assert_log_lacks 'rc-update'
run_installer "$CASE_DIR/restart-output" --server https://monitor.example.com --version 0.1.0 </dev/null
assert_log_has 'rc-service orbitbeacon-agent restart'
assert_log_has 'rc-update add orbitbeacon-agent default'
pass 'OpenRC is selected automatically and no-start leaves runlevels and processes unchanged'

# 执行已安装的 supervisor；验证真实退出码与信号，不检查源码文本。
supervise=$TEST_ROOT/opt/orbit-beacon/libexec/orbitbeacon-agent-supervise
supervisor_root=$CASE_DIR/supervisor
mkdir -p "$supervisor_root/bin" "$supervisor_root/etc" "$supervisor_root/libexec" "$supervisor_root/state"
cp "$TEST_ROOT/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback" "$supervisor_root/libexec/"
cat >"$supervisor_root/bin/orbitbeacon-agent" <<'AGENT'
#!/bin/sh
root=$SUPERVISOR_ROOT
if [ -f "$root/state/next" ]; then
	code=$(cat "$root/state/next")
	rm "$root/state/next"
	printf '%s\n' first >>"$root/state/starts"
	exit "$code"
fi
printf '%s\n' restarted >>"$root/state/starts"
exit 0
AGENT
chmod 755 "$supervisor_root/bin/orbitbeacon-agent"
printf 75 >"$supervisor_root/state/next"
printf 0 >"$supervisor_root/state/upgrade-pending"
SUPERVISOR_ROOT=$supervisor_root "$supervise" "$supervisor_root"
[ "$(cat "$supervisor_root/state/upgrade-pending")" = 2 ] || fail 'rollback was not checked on every startup'
[ "$(wc -l <"$supervisor_root/state/starts")" = 2 ] || fail 'upgrade exit did not restart exactly once'
printf 3 >"$supervisor_root/state/next"
if SUPERVISOR_ROOT=$supervisor_root "$supervise" "$supervisor_root"; then fail 'identity conflict status lost'; else [ "$?" = 3 ] || fail 'identity conflict code changed'; fi
[ "$(wc -l <"$supervisor_root/state/starts")" = 3 ] || fail 'identity conflict restarted'
pass 'OpenRC supervisor restarts upgrades with rollback checks and stops on zero or identity conflict'

cat >"$supervisor_root/bin/orbitbeacon-agent" <<'AGENT'
#!/bin/sh
trap 'printf reload >"$SUPERVISOR_ROOT/state/reloaded"' HUP
trap 'exit 0' TERM INT
printf '%s\n' "$$" >"$SUPERVISOR_ROOT/state/child.pid"
while :; do sleep 0.1; done
AGENT
python3 - "$supervise" "$supervisor_root" <<'PY'
import os, pathlib, signal, subprocess, sys, time
supervise, root = sys.argv[1:]
state = pathlib.Path(root) / 'state'
proc = subprocess.Popen([supervise, root], env={**os.environ, 'SUPERVISOR_ROOT': root})
child = None
def await_file(name):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        path = state / name
        if path.exists() and path.read_text():
            return path.read_text()
        assert proc.poll() is None, 'supervisor exited before signal handling'
        time.sleep(0.02)
    raise AssertionError(f'timed out waiting for {name}')
try:
    child = int(await_file('child.pid'))
    proc.send_signal(signal.SIGHUP)
    assert await_file('reloaded') == 'reload'
    assert int((state / 'child.pid').read_text()) == child, 'reload restarted Agent'
    proc.terminate()
    assert proc.wait(timeout=5) == 0
    try:
        os.kill(child, 0)
    except ProcessLookupError:
        pass
    else:
        raise AssertionError('stopping supervisor left Agent running')
finally:
    if proc.poll() is None:
        proc.kill()
        proc.wait()
    if child is not None:
        try:
            os.kill(child, signal.SIGKILL)
        except ProcessLookupError:
            pass
PY
pass 'OpenRC supervisor forwards reload without restarting and terminates its Agent child'

run_installer "$CASE_DIR/uninstall-output" --uninstall --yes
assert_log_has 'rc-service --ifstarted orbitbeacon-agent stop'
assert_log_has 'rc-update del orbitbeacon-agent default'
assert_not_exists "$TEST_ROOT/etc/init.d/orbitbeacon-agent"
assert_not_exists "$TEST_ROOT/opt/orbit-beacon"
pass 'OpenRC uninstall stops and disables before deleting the installation'

printf '1..%s\n' "$PASS_COUNT"
