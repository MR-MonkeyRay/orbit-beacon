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
assert_log_has "systemctl enable --now orbitbeacon-agent"
assert_log_has "--config $TEST_ROOT/opt/orbit-beacon/etc/agent.yaml"
assert_log_lacks "$secret"
if grep -F -- "$secret" "$CASE_DIR/output" >/dev/null 2>&1; then fail "token leaked to installer output"; fi
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

for machine in x86_64 aarch64 arm64; do
	setup_case "architecture_$machine"
	case $machine in
		x86_64) expected_arch=amd64; other_arch=arm64 ;;
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
for tool in dash awk stat mkdir mktemp chmod cp install cat sha256sum stty rm cut sed; do
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
for invalid_base in http://release.example.com https://user@release.example.com https://release.example.com/?q=x https://release.example.com/#x; do
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url "$invalid_base" </dev/null; then fail 'invalid upgrade source accepted'; fi
done
run_with_token update-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://release.example.com/download/
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == "https://release.example.com/download"'
assert_log_has 'https://release.example.com/download/v0.1.0/'
pass 'custom update source is HTTPS only and normalized'
run_with_token update-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == "https://release.example.com/download"'
assert_log_has 'https://release.example.com/download/v0.1.0/'
pass 'reinstall keeps the pinned update source'
reinstall_policy=$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml
for update_source in 'https://release.example.com/download' ''; do
    python3 - "$reinstall_policy" "$update_source" <<'EDIT'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
lines = path.read_text().splitlines()
path.write_text('\n'.join('update_base_url: ' + json.dumps(sys.argv[2]) if line.startswith('update_base_url:') else line for line in lines) + '\n')
EDIT
    run_with_token update-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
    "$YAML_DECODER" "$reinstall_policy" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == sys.argv[1]' "$update_source"
done
pass 'reinstall keeps the generated YAML update source, including explicitly disabled upgrades'

for unsupported in missing duplicate complex; do
    cp "$reinstall_policy" "$CASE_DIR/policy-before.yaml"
    python3 - "$reinstall_policy" "$unsupported" <<'EDIT'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
lines = [line for line in path.read_text().splitlines() if not line.startswith('update_base_url:')]
if sys.argv[2] == 'duplicate':
    lines += ['update_base_url: ""', 'update_base_url: "https://release.example.com/download"']
elif sys.argv[2] == 'complex':
    lines += ['update_base_url: >-', '  https://release.example.com/download']
path.write_text('\n'.join(lines) + '\n')
EDIT
    cp "$reinstall_policy" "$CASE_DIR/policy-edited.yaml"
    : >"$TEST_LOG"
    if run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 </dev/null; then fail "ambiguous update source accepted: $unsupported"; fi
    cmp -s "$reinstall_policy" "$CASE_DIR/policy-edited.yaml" || fail 'failed source extraction modified policy'
    assert_log_lacks 'curl'
    assert_log_lacks 'chown'
    grep -F -- '--update-base-url' "$CASE_DIR/output" >/dev/null || fail 'source extraction error omitted recovery instructions'
    cp "$CASE_DIR/policy-before.yaml" "$reinstall_policy"
done
pass 'unsupported YAML update source fails before mutation with explicit override instructions'

printf '%s\n' 'update_base_url: >-' '  https://release.example.com/download' >"$reinstall_policy"
run_installer "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://override.example.com/download </dev/null
"$YAML_DECODER" "$reinstall_policy" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == "https://override.example.com/download"'
pass 'explicit update source safely replaces unsupported existing policy syntax'

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
run_installer "$CASE_DIR/policy-output" --server https://monitor.example.com --version 0.1.0 --update-base-url https://legacy.example.com/download </dev/null
cmp -s "$TEST_ROOT/opt/orbit-beacon/etc/agent.json" "$TEST_ROOT/opt/orbit-beacon/etc/agent.yaml" || fail 'policy migration replaced runtime settings'
[ "$(cat "$TEST_ROOT/opt/orbit-beacon/state/identity.json")" = '{"node_id":"copied-identity"}' ] || fail 'policy migration replaced identity'
"$YAML_DECODER" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml" | python3 -c 'import json, sys; assert json.load(sys.stdin)["update_base_url"] == "https://legacy.example.com/download"'
pass 'copied JSON policy supports reinstall with an explicit update source'

setup_case policy_symlink
mkdir -p "$TEST_ROOT/opt/orbit-beacon/etc"
printf '%s\n' untouched >"$CASE_DIR/policy-target"
ln -s "$CASE_DIR/policy-target" "$TEST_ROOT/opt/orbit-beacon/etc/install-policy.yaml"
if run_with_token policy-token "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then fail 'symlinked YAML policy accepted'; fi
[ "$(cat "$CASE_DIR/policy-target")" = untouched ] || fail 'symlinked policy target modified'
assert_log_lacks 'chown'
assert_log_lacks 'curl'
pass 'YAML policy symlink is rejected before mutation'

printf '1..%s\n' "$PASS_COUNT"
