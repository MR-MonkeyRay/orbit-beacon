#!/bin/sh
# shellcheck disable=SC2016
set -eu

# Integration tests for install-agent.sh. Every installer invocation is guarded by
# its fake-root test mode; this test never invokes sudo or writes to host paths.
# Stub bodies are deliberately single-quoted so their variables expand when the
# generated stub runs, rather than while this harness writes it.

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
	mkdir -p "$TEST_ROOT/var/lib/orbitbeacon-agent"
	printf '%s\n' '{"node_id":"test-node"}' >"$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
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
		passwd:orbitbeacon-agent) printf "%s\\n" "orbitbeacon-agent:x:12345:12346::/var/lib/orbitbeacon-agent:/usr/sbin/nologin" ;;
		group:orbitbeacon-agent) printf "%s\\n" "orbitbeacon-agent:x:12346:" ;;
		*) exit 2 ;;
	esac
else
	exit 2
fi'
	write_stub chown 'printf "chown" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"'
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
shift 2
[ "${1:-}" = -- ] || exit 2
shift
exec "$@"'
	write_stub su '
printf "su" >>"$TEST_LOG"; printf " %s" "$@" >>"$TEST_LOG"; printf "\\n" >>"$TEST_LOG"
command_string=
while [ "$#" -gt 0 ]; do
	case $1 in -c) command_string=$2; shift 2 ;; *) shift ;; esac
done
[ -n "$command_string" ] || exit 2
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
assert_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
assert_exists "$TEST_ROOT/etc/orbitbeacon/install-policy.json"
assert_exists "$TEST_ROOT/etc/orbitbeacon/agent.json"
assert_mode "$TEST_ROOT/etc/orbitbeacon/agent.json" 640
[ "$(cat "$TEST_ROOT/etc/orbitbeacon/agent.json")" = '{}' ] || fail "new runtime configuration is not empty defaults"
assert_log_has "chown root:12346"
assert_exists "$TEST_ROOT/etc/orbitbeacon/ca.pem"
assert_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_exists "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
assert_mode "$TEST_ROOT/usr/local/bin/orbitbeacon-agent" 755
assert_mode "$TEST_ROOT/etc/orbitbeacon" 755
assert_mode "$TEST_ROOT/etc/orbitbeacon/install-policy.json" 644
assert_mode "$TEST_ROOT/etc/orbitbeacon/ca.pem" 644
assert_mode "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service" 644
assert_mode "$TEST_ROOT/var/lib/orbitbeacon-agent" 700
cmp -s "$UNIT_SOURCE" "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service" || fail "installed unit differs from repository unit"
python3 - "$TEST_ROOT/etc/orbitbeacon/install-policy.json" "$TEST_ROOT" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
root = sys.argv[2]
data = json.loads(p.read_text())
assert data == {
    "server_url": "https://monitor.example.com",
    "ca_file": root + "/etc/orbitbeacon/ca.pem",
    "state_dir": root + "/var/lib/orbitbeacon-agent",
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
assert_log_has "--config $TEST_ROOT/etc/orbitbeacon/agent.json"
assert_log_lacks "$secret"
if grep -F -- "$secret" "$CASE_DIR/output" >/dev/null 2>&1; then fail "token leaked to installer output"; fi
pass "download install creates valid policy, secure files, synced unit, and does not leak token"

printf '%s\n' '{"node_id":"identity-must-survive"}' >"$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
printf '%s\n' '{"report_interval_seconds":5,"probe_targets":[]}' >"$TEST_ROOT/etc/orbitbeacon/agent.json"
before_enrollments=$(wc -l <"$TEST_ROOT/enrollments.log")
: >"$TEST_LOG"
run_installer "$CASE_DIR/upgrade-output" --server https://monitor.example.com --version 0.1.0 </dev/null
grep -F 'identity-must-survive' "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json" >/dev/null || fail "upgrade replaced identity"
[ "$(cat "$TEST_ROOT/etc/orbitbeacon/agent.json")" = '{"report_interval_seconds":5,"probe_targets":[]}' ] || fail "upgrade replaced local runtime settings"
after_enrollments=$(wc -l <"$TEST_ROOT/enrollments.log")
[ "$before_enrollments" = "$after_enrollments" ] || fail "tokenless upgrade enrolled again"
assert_log_has "systemctl restart orbitbeacon-agent"
pass "tokenless re-run preserves identity and restarts upgraded service"

for machine in x86_64 aarch64 arm64; do
	setup_case "architecture_$machine"
	case $machine in
		x86_64) expected_arch=amd64; other_arch=arm64 ;;
		*) expected_arch=arm64; other_arch=amd64 ;;
	esac
	TEST_ARCH=$machine TEST_BAD_CHECKSUM_ARCH=$other_arch run_with_token 'architecture-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
	cmp -s "$TEST_FIXTURES/orbitbeacon-agent-linux-$expected_arch" "$TEST_ROOT/usr/local/bin/orbitbeacon-agent" || fail "$machine installed the wrong architecture"
	assert_exists "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
	pass "$machine installs its architecture-only binary and ignores unrelated checksums"
done

setup_case config_symlink
mkdir -p "$TEST_ROOT/etc/orbitbeacon"
printf '%s\n' '{"report_interval_seconds":5}' >"$CASE_DIR/external.json"
ln -s "$CASE_DIR/external.json" "$TEST_ROOT/etc/orbitbeacon/agent.json"
if run_with_token 'symlink-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "installer accepted a symlinked runtime configuration"
fi
[ "$(cat "$CASE_DIR/external.json")" = '{"report_interval_seconds":5}' ] || fail "installer modified symlink target"
pass "runtime configuration symlink is rejected without changing its target"

setup_case config_directory
mkdir -p "$TEST_ROOT/etc/orbitbeacon/agent.json"
if run_with_token 'directory-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "installer accepted a directory as runtime configuration"
fi
assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
pass "non-regular runtime configuration is rejected before installation"

setup_case foreground
rm -rf -- "$TEST_ROOT/run/systemd"
run_with_token 'foreground-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0
grep -F -- "run --install-policy $TEST_ROOT/etc/orbitbeacon/install-policy.json --config $TEST_ROOT/etc/orbitbeacon/agent.json" "$CASE_DIR/output" >/dev/null || fail "foreground instructions omitted local configuration"
assert_log_lacks "systemctl enable"
pass "non-systemd instructions use the installed local configuration"

for machine in x86_64 aarch64 arm64; do
	setup_case "checksum_$machine"
	case $machine in
		x86_64) expected_arch=amd64 ;;
		*) expected_arch=arm64 ;;
	esac
	if TEST_ARCH=$machine TEST_BAD_CHECKSUM_ARCH=$expected_arch run_with_token 'checksum-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
		fail "$machine bad checksum unexpectedly succeeded"
	fi
	grep -F 'Checksum verification failed; nothing installed.' "$CASE_DIR/output" >/dev/null || fail "$machine did not verify the selected architecture checksum"
	assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
	assert_log_lacks "runuser"
	pass "$machine checksum failure stops before binary installation or enrollment"
done

setup_case enroll_failure
failure_secret='failure-token-DO-NOT-LOG'
if TEST_ENROLL_FAIL=1 run_with_token "$failure_secret" "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "enrollment failure unexpectedly succeeded"
fi
token_path=$TEST_ROOT/var/lib/orbitbeacon-agent/enrollment.token
assert_exists "$token_path"
assert_mode "$token_path" 600
assert_mode "$TEST_ROOT/var/lib/orbitbeacon-agent" 700
[ "$(sed -n '1p' "$token_path")" = "$failure_secret" ] || fail "retained token content changed"
assert_log_lacks "$failure_secret"
if grep -F -- "$failure_secret" "$CASE_DIR/output" >/dev/null 2>&1; then fail "failure token leaked to output"; fi
pass "failed enrollment retains a mode-0600 token without leaking it"

setup_case local_binary
local_secret='local-token-DO-NOT-LOG'
printf '%s\n' "$local_secret" >"$TEST_FIXTURES/enrollment.token"
chmod 600 "$TEST_FIXTURES/enrollment.token"
run_installer "$CASE_DIR/output" --server https://monitor.example.com --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64" --token-file "$TEST_FIXTURES/enrollment.token"
assert_exists "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
assert_log_lacks "curl"
assert_log_lacks "$local_secret"
pass "local binary install skips release downloads"

http_case=0
for http_server in 'http://monitor.example.com/' 'http://192.0.2.10:8080' 'http://[2001:db8::1]:8080'; do
	http_case=$((http_case + 1))
	setup_case "http_url_$http_case"
	run_with_token 'http-token' "$CASE_DIR/output" --server "$http_server" --version 0.1.0
	python3 - "$TEST_ROOT/etc/orbitbeacon/install-policy.json" "${http_server%/}" <<'PY'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert data["server_url"] == sys.argv[2], data
assert data["ca_file"] == "", data
PY
	assert_exists "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
	assert_log_has "--proto =https"
	assert_log_has "--proto-redir =https"
	assert_log_has "https://github.com/mr-monkeyray/orbit-beacon/releases/download/v0.1.0/"
done
pass "ordinary HTTP server origins are accepted without weakening release download TLS"

setup_case http_ca_conflict
if run_with_token 'http-ca-token' "$CASE_DIR/output" --server http://monitor.example.com --version 0.1.0 --ca-file "$TEST_FIXTURES/missing.pem"; then
	fail "HTTP server accepted a CA file"
fi
grep -F -- '--ca-file requires an HTTPS server.' "$CASE_DIR/output" >/dev/null || fail "HTTP CA conflict was not reported before reading the CA file"
assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
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
assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
pass "server URLs with userinfo, malformed host, paths, queries, or JSON-unsafe characters are rejected"

setup_case ipv6_url
run_with_token 'ipv6-token' "$CASE_DIR/output" --server 'https://[2001:db8::1]:8443' --binary "$TEST_FIXTURES/orbitbeacon-agent-linux-amd64"
python3 - "$TEST_ROOT/etc/orbitbeacon/install-policy.json" <<'PY'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert data["server_url"] == "https://[2001:db8::1]:8443", data
PY
pass "valid bracketed IPv6 HTTPS origin is accepted and JSON encoded"

setup_case uid_zero
: >"$TEST_ROOT/.account-created"
if TEST_ACCOUNT_UID0=1 run_with_token 'uid-zero-token' "$CASE_DIR/output" --server https://monitor.example.com --version 0.1.0; then
	fail "uid-zero account unexpectedly accepted"
fi
assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
pass "uid-zero agent account is rejected"

setup_case uninstall
run_with_token 'uninstall-token' "$CASE_DIR/install-output" --server https://monitor.example.com --version 0.1.0 --ca-file "$TEST_FIXTURES/ca.pem"
printf 'n\n' | env PATH="$TEST_PATH" dash "$INSTALLER" --uninstall >"$CASE_DIR/refuse-output" 2>&1 || true
assert_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
assert_exists "$TEST_ROOT/var/lib/orbitbeacon-agent/identity.json"
run_installer "$CASE_DIR/uninstall-output" --uninstall --yes
assert_not_exists "$TEST_ROOT/usr/local/bin/orbitbeacon-agent"
assert_not_exists "$TEST_ROOT/etc/orbitbeacon/install-policy.json"
assert_not_exists "$TEST_ROOT/etc/orbitbeacon/agent.json"
assert_not_exists "$TEST_ROOT/etc/orbitbeacon/ca.pem"
assert_not_exists "$TEST_ROOT/etc/systemd/system/orbitbeacon-agent.service"
assert_not_exists "$TEST_ROOT/var/lib/orbitbeacon-agent"
assert_log_has "systemctl disable --now orbitbeacon-agent"
assert_log_has "systemctl daemon-reload"
pass "uninstall refuses declined confirmation and --yes removes installed state"

printf '1..%s\n' "$PASS_COUNT"
