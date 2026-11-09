#!/bin/sh
# 可选 Docker smoke；只检查安装、保留及真实工具兼容，不测试注册或完整 init。
set -eu

usage() {
    cat <<'USAGE'
用法：sh deploy/install-agent.docker-test.sh [镜像 ...]
默认：debian:12-slim ubuntu:24.04 quay.io/centos/centos:stream9 alpine:3.21
需要宿主 Go、可用的本地 Docker daemon 和联网的软件仓库；只构建当前 amd64/arm64 架构。
容器无特权，宿主脚本和二进制只读挂载，安装仅写入一次性容器。
检查离线安装、UID/GID、权限、重装保留、非 root version 和服务文件；不验证注册或服务运行。
USAGE
}

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

container_smoke() {
    # 防止误把内部入口当作宿主安装命令。
    if [ ! -f /.dockerenv ] || [ ! -f /smoke/install-agent.sh ] || [ ! -x /smoke/orbitbeacon-agent ]; then
        fail '内部入口仅供测试容器使用。'
    fi
    expected_arch=$1
    export LC_ALL=C
    unset ORBITBEACON_INSTALL_TEST ORBITBEACON_INSTALL_ROOT
    # 使用真实系统工具；Alpine 保留 BusyBox 账户工具，补齐 install 等工具。
    if command -v apk >/dev/null 2>&1; then
        apk add --no-cache openrc coreutils
        service_manager=openrc
    elif command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends passwd util-linux coreutils systemd procps
        service_manager=systemd
    elif command -v dnf >/dev/null 2>&1; then
        # CentOS minimal 的 coreutils-single 已提供所需工具，不强制替换为冲突的 coreutils。
        dnf install -y shadow-utils util-linux systemd procps-ng diffutils
        service_manager=systemd
    else
        fail '镜像须支持 apk、apt-get 或 dnf。'
    fi
    for tool in chown install mv stat su cmp; do
        command -v "$tool" >/dev/null 2>&1 || fail "缺少真实工具：$tool"
    done

    account=orbitbeacon-agent
    agent_uid=23456
    agent_gid=23457
    root=/opt/orbit-beacon
    identity=$root/state/identity.json
    config=$root/etc/agent.yaml
    policy=$root/etc/install-policy.yaml
    binary=$root/bin/orbitbeacon-agent
    unit=/etc/systemd/system/orbitbeacon-agent.service
    init_script=/etc/init.d/orbitbeacon-agent
    ! id "$account" >/dev/null 2>&1 || fail '镜像已存在测试账户。'
    [ ! -e "$root" ] || fail '镜像已存在安装目录。'

    # 普通文件仅触发“保留身份”；不是有效凭据，也不代表完成注册。
    mkdir -p "$root/state"
    printf '%s\n' 'docker-smoke-preservation-marker-not-an-enrolled-identity' >/tmp/identity.expected
    cp /tmp/identity.expected "$identity"
    chown "$agent_uid:$agent_gid" "$identity"
    chmod 0600 "$identity"

    install_agent() {
        sh /smoke/install-agent.sh --server https://monitor.example.com \
            --binary /smoke/orbitbeacon-agent --service "$1" --no-start \
            --uid "$agent_uid" --gid "$agent_gid" </dev/null
    }
    assert_metadata() {
        actual=$(stat -c '%a:%u:%g' "$1")
        [ "$actual" = "$2" ] || fail "$1 权限/属主：期望 $2，实际 $actual"
    }
    assert_preserved() {
        cmp /tmp/identity.expected "$identity" || fail '身份标记被改写。'
        cmp /tmp/config.expected "$config" || fail '手改配置被改写。'
        [ "$(id -u "$account")" = "$agent_uid" ] || fail 'UID 被改写。'
        [ "$(id -g "$account")" = "$agent_gid" ] || fail 'GID 被改写。'
    }
    assert_permissions() {
        assert_metadata "$root" "750:0:$agent_gid"
        assert_metadata "$root/bin" "750:$agent_uid:$agent_gid"
        assert_metadata "$binary" "750:$agent_uid:$agent_gid"
        assert_metadata "$root/state" "700:$agent_uid:$agent_gid"
        assert_metadata "$identity" "600:$agent_uid:$agent_gid"
        assert_metadata "$root/etc" "750:0:$agent_gid"
        assert_metadata "$config" "640:0:$agent_gid"
        assert_metadata "$policy" "640:0:$agent_gid"
        assert_metadata "$root/libexec" '755:0:0'
        assert_metadata "$root/libexec/orbitbeacon-agent-rollback" '755:0:0'
        grep -Eq "^expected_uid: $agent_uid$" "$policy" || fail '策略 UID 不匹配。'
        grep -Eq "^expected_gid: $agent_gid$" "$policy" || fail '策略 GID 不匹配。'
    }

    install_agent none
    if [ -e "$unit" ] || [ -e "$init_script" ]; then
        fail '--service none 安装了服务。'
    fi
    [ ! -e "$root/state/enrollment.token" ] || fail '无注册输入却生成了票据。'
    cmp /tmp/identity.expected "$identity" || fail '初次安装改写了身份标记。'
    assert_permissions
    printf '%s\n' 'report_interval_seconds: 17' 'interfaces: ["lo"]' '# docker smoke 手改配置' >"$config"
    cp "$config" /tmp/config.expected
    install_agent none
    assert_preserved
    assert_permissions

    # 真实切换账户并执行真实二进制，而非命令桩。
    su -s /bin/sh "$account" -c "test \"\$(id -u)\" = '$agent_uid' && test \"\$(id -g)\" = '$agent_gid' && exec '$binary' version --json" >/tmp/version.json
    grep -q '"version":' /tmp/version.json || fail '缺少 JSON 版本。'
    grep -q '"os":"linux"' /tmp/version.json || fail '二进制不是 Linux 构建。'
    grep -q "\"arch\":\"$expected_arch\"" /tmp/version.json || fail '二进制架构不匹配。'
    cat /tmp/version.json

    for conflict in uid gid; do
        requested_uid=$agent_uid
        requested_gid=$agent_gid
        case $conflict in
            uid) requested_uid=23458 ;;
            gid) requested_gid=23459 ;;
        esac
        if sh /smoke/install-agent.sh --server https://monitor.example.com \
            --binary /smoke/orbitbeacon-agent --service none --no-start \
            --uid "$requested_uid" --gid "$requested_gid" </dev/null >/tmp/conflict.log 2>&1; then
            fail "已有账户的 $conflict 冲突未拒绝。"
        fi
        assert_preserved
        assert_permissions
    done

    install_agent "$service_manager"
    assert_preserved
    assert_permissions
    if [ "$service_manager" = openrc ]; then
        assert_metadata "$init_script" '755:0:0'
        sh -n "$init_script"
        rc-service --exists orbitbeacon-agent || fail 'OpenRC 无法识别服务。'
        rc-service orbitbeacon-agent describe || fail 'OpenRC 服务描述加载失败。'
        [ -z "$(find /etc/runlevels -name orbitbeacon-agent -print)" ] || fail '--no-start 将服务加入了 OpenRC 启动集。'
    else
        assert_metadata "$unit" '644:0:0'
        if command -v systemd-analyze >/dev/null 2>&1; then
            systemd-analyze verify "$unit"
        else
            printf '%s\n' 'SKIP: 镜像没有 systemd-analyze，未验证 unit 语法。'
        fi
        [ -z "$(find /etc/systemd/system -type l -name orbitbeacon-agent.service -print)" ] || fail '--no-start 启用了 systemd 服务。'
    fi
    # 只执行 version；Agent 不应因 --no-start 出现在进程表中。
    for comm in /proc/[0-9]*/comm; do
        [ -r "$comm" ] || continue
        IFS= read -r process_name <"$comm" || continue
        if [ "$process_name" = orbitbeacon-age ] || [ "$process_name" = orbitbeacon-agent ]; then
            fail '--no-start 启动了 Agent。'
        fi
    done
    printf '%s\n' 'PASS: 安装/保留/真实工具兼容检查完成；身份仅为保留标记，未测试注册、网络或完整 init。'
}

if [ "${1:-}" = --container ]; then
    [ "$#" = 2 ] || fail '内部入口缺少架构。'
    container_smoke "$2"
    exit 0
fi
case ${1:-} in
    -h|--help) usage; exit 0 ;;
esac
for image in "$@"; do
    case $image in ''|-*) fail '请传入镜像名，或使用 --help。' ;; esac
done
command -v docker >/dev/null 2>&1 || fail '需要 Docker。'
command -v go >/dev/null 2>&1 || fail '需要 Go。'
repo_root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd -P)
arch=$(go env GOHOSTARCH)
case $arch in amd64|arm64) ;; *) fail "仅测试 amd64/arm64，当前为 $arch。" ;; esac
docker info >/dev/null
work=$(mktemp -d "${TMPDIR:-/tmp}/orbitbeacon-docker-smoke.XXXXXXXX")
cleanup() {
    if [ -s "$work/container.cid" ]; then
        IFS= read -r container_id <"$work/container.cid" || :
        [ -z "${container_id:-}" ] || docker rm -f "$container_id" >/dev/null 2>&1 || :
    fi
    rm -rf "$work"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM
(
    cd "$repo_root"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$work/orbitbeacon-agent" ./cmd/orbitbeacon-agent
)
if [ "$#" = 0 ]; then
    set -- debian:12-slim ubuntu:24.04 quay.io/centos/centos:stream9 alpine:3.21
fi
for image in "$@"; do
    printf '\n== %s (linux/%s) ==\n' "$image" "$arch"
    docker run --rm --platform "linux/$arch" --cidfile "$work/container.cid" \
        --mount "type=bind,src=$repo_root/deploy/install-agent.sh,dst=/smoke/install-agent.sh,readonly" \
        --mount "type=bind,src=$repo_root/deploy/install-agent.docker-test.sh,dst=/smoke/docker-test.sh,readonly" \
        --mount "type=bind,src=$work/orbitbeacon-agent,dst=/smoke/orbitbeacon-agent,readonly" \
        --entrypoint /bin/sh "$image" /smoke/docker-test.sh --container "$arch"
    rm -f "$work/container.cid"
done
